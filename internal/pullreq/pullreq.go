// Package pullreq materializes a pull request's head for review without
// touching the operator's checkout.
//
// The head comes from the GitHub-style refs/pull/<n>/head ref on a configured
// remote. It is fetched into a ref under refs/roast/pull/, checked out into a
// throwaway linked worktree, and both are removed by Close. The base defaults
// to the remote's HEAD, fetched in the same call so a stale remote-tracking
// branch cannot widen the diff with commits that landed after the PR forked.
package pullreq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RefPrefix holds the fetched head and base refs for the lifetime of one
// review. Each review gets its own subdirectory, so concurrent reviews of the
// same pull request never share or delete each other's refs.
const RefPrefix = "refs/roast/pull/"

// redirectingGitEnv names variables that point git at another repository,
// worktree or index even under `git -C`. Inherited from a hook or a wrapper,
// they would make read-tree and checkout-index write the pull request over
// the operator's index or files.
var redirectingGitEnv = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"}

// UsageError reports a request that cannot be satisfied in this repository
// regardless of network state, such as an unknown remote.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

// Checkout is a pull request head checked out for review.
type Checkout struct {
	Dir     string // linked worktree, detached at Head, with a clean index
	Remote  string
	Number  int
	Head    string // pull request head commit
	Base    string // base commit: the explicit base, else the remote's HEAD
	BaseRef string // what Base was resolved from, for display and run records

	repo     string
	tmp      string
	adminDir string // the worktree's administrative directory under the common git dir
	refs     []string
}

// Prepare fetches pull request number from remote and checks its head out
// into a temporary linked worktree of repo. base, when non-empty, is a local
// ref used as-is; otherwise the remote's HEAD is fetched alongside the head.
// The caller must Close the result. On error nothing is left behind.
func Prepare(ctx context.Context, repo, remote string, number int, base string) (*Checkout, error) {
	for _, name := range redirectingGitEnv {
		if os.Getenv(name) != "" {
			return nil, &UsageError{msg: fmt.Sprintf("--pr cannot run with %s set; it would redirect the pull request checkout into another repository, worktree or index", name)}
		}
	}
	if number <= 0 {
		return nil, &UsageError{msg: fmt.Sprintf("pull request number must be positive, got %d", number)}
	}
	if err := checkRemote(repo, remote); err != nil {
		return nil, err
	}
	if strings.HasPrefix(base, "-") {
		return nil, &UsageError{msg: fmt.Sprintf("base ref %q cannot start with '-'", base)}
	}

	tmp, err := os.MkdirTemp("", "roast-pr-")
	if err != nil {
		return nil, fmt.Errorf("create pull request worktree directory: %w", err)
	}
	c := &Checkout{
		Dir: filepath.Join(tmp, "worktree"), Remote: remote, Number: number,
		repo: repo, tmp: tmp,
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.Close()
		}
	}()

	// The temp directory's random suffix keys this review's refs.
	ns := RefPrefix + filepath.Base(tmp) + "/"
	headRef := ns + "head"
	refspecs := []string{fmt.Sprintf("+refs/pull/%d/head:%s", number, headRef)}
	baseRef := ""
	if base == "" {
		baseRef = ns + "base"
		refspecs = append(refspecs, "+HEAD:"+baseRef)
	}
	// Record the refs before fetching: a fetch can fail after writing one of
	// them, and Close must still delete it.
	c.refs = append(c.refs, headRef)
	if baseRef != "" {
		c.refs = append(c.refs, baseRef)
	}
	// --refmap= stops remote.<name>.fetch mappings from also updating
	// remote-tracking refs, and --no-prune stops fetch.prune from deleting
	// any. Only the per-run refs above change.
	fetchArgs := append([]string{
		"-c", "fetch.recurseSubmodules=false",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--refmap=", "--no-prune", remote,
	}, refspecs...)
	if _, err := git(ctx, repo, fetchArgs...); err != nil {
		return nil, fmt.Errorf("fetch pull request #%d from %s (the remote must publish refs/pull/<n>/head, as GitHub does): %w", number, remote, err)
	}

	if c.Head, err = commitOID(repo, headRef); err != nil {
		return nil, err
	}
	if base == "" {
		c.BaseRef = remote + "/HEAD"
		if c.Base, err = commitOID(repo, baseRef); err != nil {
			return nil, err
		}
	} else {
		c.BaseRef = base
		if c.Base, err = commitOID(repo, base); err != nil {
			return nil, &UsageError{msg: fmt.Sprintf("base ref %q does not resolve to a commit in this repository", base)}
		}
	}

	// Without a merge base, the diff from base to head would include every
	// difference between the two trees, not the pull request's changes.
	if _, err := git(ctx, repo, "merge-base", c.Base, c.Head); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("pull request #%d head %s has no merge base with %s; in a shallow clone, run `git fetch --unshallow` and retry", number, c.Head, c.BaseRef)
	}

	// --no-checkout keeps `worktree add` from running the post-checkout hook
	// on contributor-controlled content; read-tree and checkout-index then
	// populate the index and files, as snapshot.Restore does.
	if _, err := git(ctx, repo, "worktree", "add", "--detach", "--no-checkout", c.Dir, c.Head); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	if c.adminDir, err = git(ctx, c.Dir, "rev-parse", "--absolute-git-dir"); err != nil {
		return nil, fmt.Errorf("locate worktree git directory: %w", err)
	}
	if _, err := git(ctx, c.Dir, "read-tree", c.Head); err != nil {
		return nil, fmt.Errorf("read-tree: %w", err)
	}
	if _, err := git(ctx, c.Dir, "checkout-index", "-a", "-f"); err != nil {
		return nil, fmt.Errorf("checkout-index: %w", err)
	}
	ok = true
	return c, nil
}

// Close removes the worktree, its directory, and the fetched refs. It runs
// every step even after a failure and reports the first error. It ignores
// cancellation so an interrupted review still cleans up.
func (c *Checkout) Close() error {
	var errs []error
	_, statErr := os.Stat(c.Dir)
	// The entry outlives its directory if something deletes the directory
	// mid-review, so remove it whenever the worktree was registered.
	if c.adminDir != "" || statErr == nil {
		if _, err := git(context.Background(), c.repo, "worktree", "remove", "--force", c.Dir); err != nil {
			// Remove only this worktree's administrative entry. `git
			// worktree prune` would also drop the operator's stale entries.
			if c.adminDir == "" || filepath.Base(filepath.Dir(c.adminDir)) != "worktrees" {
				errs = append(errs, err)
			} else if rmErr := os.RemoveAll(c.adminDir); rmErr != nil {
				errs = append(errs, rmErr)
			}
		}
	}
	if err := os.RemoveAll(c.tmp); err != nil {
		errs = append(errs, err)
	}
	for _, ref := range c.refs {
		if _, err := git(context.Background(), c.repo, "update-ref", "-d", ref); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkRemote accepts only a configured remote name. That rules out URLs and
// option-shaped arguments reaching `git fetch`, and gives a clear error before
// any network call.
func checkRemote(repo, remote string) error {
	out, err := git(context.Background(), repo, "remote")
	if err != nil {
		return fmt.Errorf("list remotes: %w", err)
	}
	for _, name := range strings.Fields(out) {
		if name == remote {
			return nil
		}
	}
	return &UsageError{msg: fmt.Sprintf("remote %q is not configured in this repository; pass --pr-remote", remote)}
}

func commitOID(repo, ref string) (string, error) {
	oid, err := git(context.Background(), repo, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil || oid == "" {
		return "", fmt.Errorf("%s does not resolve to a commit", ref)
	}
	return oid, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Killing git can orphan a child (ssh, upload-pack) that holds the
	// output pipe open; stop waiting for it shortly after cancellation.
	cmd.WaitDelay = time.Second
	raw, err := cmd.Output()
	if err != nil {
		// A killed git reports "signal: killed", not the cancellation.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
