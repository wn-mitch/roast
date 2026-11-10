package pullreq

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is an upstream repository publishing refs/pull/7/head, and a local
// clone of it whose origin/main is stale: upstream main gained a commit after
// the clone, and the pull request is based on that newer commit.
type fixture struct {
	upstream, local      string
	prHead, upstreamMain string
	staleMain            string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	upstream := t.TempDir()
	run(t, upstream, "init", "-b", "main")
	write(t, upstream, "app.go", "package app\n")
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "initial")

	local := filepath.Join(t.TempDir(), "local")
	run(t, "", "clone", "--quiet", upstream, local)
	configure(t, local)
	staleMain := run(t, local, "rev-parse", "origin/main")

	write(t, upstream, "landed.go", "package app\n\nfunc landed() {}\n")
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "landed after clone")
	upstreamMain := run(t, upstream, "rev-parse", "HEAD")

	run(t, upstream, "checkout", "--quiet", "-b", "feature")
	write(t, upstream, "app.go", "package app\n\nfunc pr() {}\n")
	run(t, upstream, "commit", "--quiet", "-am", "pull request change")
	prHead := run(t, upstream, "rev-parse", "HEAD")
	run(t, upstream, "update-ref", "refs/pull/7/head", prHead)
	run(t, upstream, "checkout", "--quiet", "main")

	// Local work in progress that a pull request review must not see or touch.
	write(t, local, "app.go", "package app\n\nfunc localEdit() {}\n")
	return fixture{upstream: upstream, local: local, prHead: prHead, upstreamMain: upstreamMain, staleMain: staleMain}
}

func TestPrepareChecksOutHeadAgainstFreshRemoteBase(t *testing.T) {
	fx := newFixture(t)
	c, err := Prepare(t.Context(), fx.local, "origin", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Head != fx.prHead {
		t.Errorf("Head = %s, want %s", c.Head, fx.prHead)
	}
	// The stale origin/main would add landed.go to the reviewed diff.
	if c.Base != fx.upstreamMain {
		t.Errorf("Base = %s, want upstream main %s (stale origin/main is %s)", c.Base, fx.upstreamMain, fx.staleMain)
	}
	if c.BaseRef != "origin/HEAD" {
		t.Errorf("BaseRef = %q", c.BaseRef)
	}
	if got := read(t, c.Dir, "app.go"); !strings.Contains(got, "func pr()") {
		t.Errorf("worktree app.go = %q, want the pull request's content", got)
	}
	if status := run(t, c.Dir, "status", "--porcelain"); status != "" {
		t.Errorf("worktree is not clean:\n%s", status)
	}
	if got := read(t, fx.local, "app.go"); !strings.Contains(got, "localEdit") {
		t.Errorf("local checkout was modified: %q", got)
	}
	if diff := run(t, c.Dir, "diff", "--name-only", c.Base+"...HEAD"); diff != "app.go" {
		t.Errorf("base...head touches %q, want only app.go", diff)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoResidue(t, fx.local, c)
}

func TestPrepareUsesExplicitBaseWithoutFetchingOne(t *testing.T) {
	fx := newFixture(t)
	c, err := Prepare(t.Context(), fx.local, "origin", 7, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.Base != fx.staleMain || c.BaseRef != "origin/main" {
		t.Errorf("Base = %s (%s), want explicit origin/main %s", c.Base, c.BaseRef, fx.staleMain)
	}
	if refs := run(t, fx.local, "for-each-ref", RefPrefix); strings.Contains(refs, "/base") {
		t.Errorf("explicit base still fetched a base ref:\n%s", refs)
	}
}

func TestPrepareConcurrentReviewsOfOnePullRequestAreIndependent(t *testing.T) {
	fx := newFixture(t)
	first, err := Prepare(t.Context(), fx.local, "origin", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(t.Context(), fx.local, "origin", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if got := run(t, second.Dir, "rev-parse", "HEAD"); got != fx.prHead {
		t.Errorf("second worktree HEAD = %s after first closed", got)
	}
	for _, ref := range second.refs {
		run(t, fx.local, "rev-parse", "--verify", ref)
	}
}

func TestPrepareRejectsWithoutLeavingResidue(t *testing.T) {
	fx := newFixture(t)
	injected := filepath.Join(t.TempDir(), "injected")
	for name, tc := range map[string]struct {
		remote, base string
		number       int
		usage        bool
	}{
		"unknown remote":    {remote: "upstream", number: 7, usage: true},
		"url as remote":     {remote: fx.upstream, number: 7, usage: true},
		"option as remote":  {remote: "--upload-pack=touch " + injected, number: 7, usage: true},
		"zero number":       {remote: "origin", number: 0, usage: true},
		"option as base":    {remote: "origin", number: 7, base: "--output=x", usage: true},
		"unresolvable base": {remote: "origin", number: 7, base: "no-such-ref", usage: true},
		"missing pr":        {remote: "origin", number: 8},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Prepare(t.Context(), fx.local, tc.remote, tc.number, tc.base)
			if err == nil {
				_ = c.Close()
				t.Fatal("Prepare succeeded")
			}
			var usage *UsageError
			if errors.As(err, &usage) != tc.usage {
				t.Errorf("usage error = %v, want %v: %v", !tc.usage, tc.usage, err)
			}
			if refs := run(t, fx.local, "for-each-ref", RefPrefix); refs != "" {
				t.Errorf("refs left behind:\n%s", refs)
			}
			if list := run(t, fx.local, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
				t.Errorf("worktrees left behind:\n%s", list)
			}
		})
	}
	if _, err := os.Stat(injected); err == nil {
		t.Error("option-shaped remote reached git")
	}
}

func TestPrepareDoesNotRunPostCheckoutHook(t *testing.T) {
	fx := newFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(fx.local, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Prepare(t.Context(), fx.local, "origin", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := os.Stat(marker); err == nil {
		t.Error("post-checkout hook ran on pull request content")
	}
}

func assertNoResidue(t *testing.T, repo string, c *Checkout) {
	t.Helper()
	if _, err := os.Stat(c.tmp); !os.IsNotExist(err) {
		t.Errorf("temp directory %s still exists (%v)", c.tmp, err)
	}
	if refs := run(t, repo, "for-each-ref", RefPrefix); refs != "" {
		t.Errorf("refs left behind:\n%s", refs)
	}
	if list := run(t, repo, "worktree", "list", "--porcelain"); strings.Contains(list, c.Dir) {
		t.Errorf("worktree still registered:\n%s", list)
	}
}

func configure(t *testing.T, dir string) {
	t.Helper()
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "Test")
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Each of these redirects git away from the linked worktree even under -C, so
// read-tree or checkout-index would overwrite the operator's index or files.
func TestPrepareRejectsInheritedGitLocationOverrides(t *testing.T) {
	fx := newFixture(t)
	run(t, fx.local, "add", "app.go")
	staged := run(t, fx.local, "write-tree")
	for _, name := range redirectingGitEnv {
		t.Run(name, func(t *testing.T) {
			value := filepath.Join(fx.local, ".git")
			switch name {
			case "GIT_INDEX_FILE":
				value = filepath.Join(fx.local, ".git", "index")
			case "GIT_WORK_TREE":
				value = fx.local
			}
			t.Setenv(name, value)
			c, err := Prepare(t.Context(), fx.local, "origin", 7, "")
			if err == nil {
				_ = c.Close()
			}
			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("err = %v, want a usage error", err)
			}
		})
	}
	if got := run(t, fx.local, "write-tree"); got != staged {
		t.Errorf("operator's index changed: %s, want %s", got, staged)
	}
	if got := read(t, fx.local, "app.go"); !strings.Contains(got, "localEdit") {
		t.Errorf("operator's file overwritten: %q", got)
	}
}

func TestPrepareLeavesConfiguredRemoteTrackingRefsAlone(t *testing.T) {
	fx := newFixture(t)
	run(t, fx.local, "config", "--add", "remote.origin.fetch", "+refs/pull/*/head:refs/remotes/origin/pr/*")
	run(t, fx.local, "config", "fetch.prune", "true")
	run(t, fx.local, "update-ref", "refs/remotes/origin/pr/99", fx.staleMain)
	c, err := Prepare(t.Context(), fx.local, "origin", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	refs := run(t, fx.local, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/pr/")
	if refs != "refs/remotes/origin/pr/99" {
		t.Errorf("remote-tracking pull refs changed by the review fetch:\n%s", refs)
	}
}

func TestPrepareRequiresAMergeBase(t *testing.T) {
	fx := newFixture(t)
	run(t, fx.upstream, "checkout", "--quiet", "--orphan", "unrelated")
	run(t, fx.upstream, "commit", "--quiet", "-m", "unrelated root")
	run(t, fx.upstream, "update-ref", "refs/pull/9/head", "HEAD")
	run(t, fx.upstream, "checkout", "--quiet", "-f", "main")
	c, err := Prepare(t.Context(), fx.local, "origin", 9, "")
	if err == nil {
		_ = c.Close()
		t.Fatal("Prepare accepted a pull request with no merge base")
	}
	if !strings.Contains(err.Error(), "merge base") {
		t.Errorf("err = %v, want it to name the missing merge base", err)
	}
	if refs := run(t, fx.local, "for-each-ref", RefPrefix); refs != "" {
		t.Errorf("refs left behind:\n%s", refs)
	}
}

func TestPrepareReportsCancellationAsContextError(t *testing.T) {
	fx := newFixture(t)
	// A fetch that hangs until the test cancels it.
	slow := filepath.Join(t.TempDir(), "slow-upload-pack")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, fx.local, "config", "remote.origin.uploadpack", slow)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	c, err := Prepare(ctx, fx.local, "origin", 7, "")
	if err == nil {
		_ = c.Close()
		t.Fatal("Prepare succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A failed `git worktree remove` must not prune other worktrees' stale
// administrative entries, which belong to the operator.
func TestCloseDoesNotPruneOtherWorktrees(t *testing.T) {
	fx := newFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	run(t, fx.local, "worktree", "add", "--quiet", "--detach", other)
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}
	c, err := Prepare(t.Context(), fx.local, "origin", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	// Lock the review worktree so `git worktree remove --force` (one --force)
	// fails and Close takes its fallback path.
	run(t, fx.local, "worktree", "lock", c.Dir)
	if err := c.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	list := run(t, fx.local, "worktree", "list", "--porcelain")
	if !strings.Contains(list, other) {
		t.Errorf("Close pruned another worktree's entry:\n%s", list)
	}
	if strings.Contains(list, c.Dir) {
		t.Errorf("review worktree still registered:\n%s", list)
	}
	if _, err := os.Stat(c.tmp); !os.IsNotExist(err) {
		t.Errorf("temp directory remains: %v", err)
	}
}

func TestCloseRemovesEntryWhenWorktreeDirectoryIsGone(t *testing.T) {
	fx := newFixture(t)
	c, err := Prepare(t.Context(), fx.local, "origin", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.Dir); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoResidue(t, fx.local, c)
}
