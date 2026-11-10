package cli

import (
	"context"
	"errors"
	"io"

	"github.com/bluesky-social/roast/internal/pullreq"
	"github.com/bluesky-social/roast/internal/record"
)

const defaultPullRequestRemote = "origin"

// validatePullRequestFlags rejects --pr combinations with no coherent meaning:
// a pull request is a committed range, so staged and uncommitted scopes do
// not apply, and a repository survey ignores the diff entirely.
func validatePullRequestFlags(changed func(string) bool, f *reviewFlags) error {
	if f.pr == 0 {
		if changed("pr") {
			return &usageError{msg: "--pr must be a positive pull request number"}
		}
		if changed("pr-remote") {
			return &usageError{msg: "--pr-remote requires --pr"}
		}
		return nil
	}
	if f.pr < 0 {
		return &usageError{msg: "--pr must be a positive pull request number"}
	}
	if f.staged || f.uncommittedOnly || f.scope == "repo" {
		return &usageError{msg: "--pr cannot be combined with --staged, --uncommitted-only, or --scope repo"}
	}
	return nil
}

// preparePullRequest checks the requested pull request out into a temporary
// worktree of repo. It returns nil when --pr was not given.
func preparePullRequest(ctx context.Context, f *reviewFlags, repo string, stderr io.Writer) (*pullreq.Checkout, error) {
	if f.pr == 0 {
		return nil, nil
	}
	logf(stderr, "fetching pull request #%d from %s", f.pr, f.prRemote)
	checkout, err := pullreq.Prepare(ctx, repo, f.prRemote, f.pr, f.base)
	if err != nil {
		var usage *pullreq.UsageError
		if errors.As(err, &usage) {
			return nil, &usageError{msg: usage.Error()}
		}
		return nil, err
	}
	logf(stderr, "reviewing pull request #%d: %s against %s (%s)", f.pr, shortOID(checkout.Head), checkout.BaseRef, shortOID(checkout.Base))
	return checkout, nil
}

// closePullRequest removes the temporary worktree and refs, reporting a
// failure without changing the review's outcome.
func closePullRequest(checkout *pullreq.Checkout, stderr io.Writer) {
	if checkout == nil {
		return
	}
	if err := checkout.Close(); err != nil {
		logf(stderr, "could not fully remove the pull request worktree (%v); `git worktree prune` clears what remains", err)
	}
}

func pullRequestMeta(checkout *pullreq.Checkout) *record.PullRequestMeta {
	if checkout == nil {
		return nil
	}
	return &record.PullRequestMeta{
		Remote: checkout.Remote, Number: checkout.Number,
		HeadOID: checkout.Head, BaseRef: checkout.BaseRef, BaseOID: checkout.Base,
	}
}

func shortOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}
