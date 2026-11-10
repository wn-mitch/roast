package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestPullRequestFlagConflictsAreUsageErrors(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	for _, args := range [][]string{
		{"--pr", "7", "--staged"},
		{"--pr", "7", "--uncommitted-only"},
		{"--pr", "7", "--scope", "repo"},
		{"--pr", "0"},
		{"--pr", "-3"},
		{"--pr-remote", "upstream"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			code, err := ExecuteContext(t.Context(), append([]string{"review"}, args...), io.Discard, &stderr)
			if code != ExitUsage {
				t.Fatalf("code=%d err=%v stderr=%s", code, err, stderr.String())
			}
		})
	}
}

// TestReviewPullRequestDiffsFetchedHeadNotLocalCheckout runs a full --pr
// review: the finder must see exactly the pull request's change, against the
// remote's current default branch, with neither the operator's uncommitted
// edit nor upstream commits missing from a stale origin/main leaking in.
func TestReviewPullRequestDiffsFetchedHeadNotLocalCheckout(t *testing.T) {
	recordDir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", recordDir)
	var mu sync.Mutex
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		prompts = append(prompts, string(raw))
		mu.Unlock()
		_ = testutil.WriteJSONStream(w, "chat", `{"id":"test","object":"chat.completion","created":1,"model":"finder-test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"findings","arguments":"{\"assessment\":\"NO_FINDINGS\",\"findings\":[]}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer server.Close()

	upstream := t.TempDir()
	runGit(t, upstream, "init", "-b", "main")
	runGit(t, upstream, "config", "user.email", "test@example.com")
	runGit(t, upstream, "config", "user.name", "Test")
	writeFile(t, upstream, "app.go", "package app\n")
	runGit(t, upstream, "add", ".")
	runGit(t, upstream, "commit", "-m", "initial")
	local := filepath.Join(t.TempDir(), "local")
	runGit(t, "", "clone", "--quiet", upstream, local)
	runGit(t, local, "config", "user.email", "test@example.com")
	runGit(t, local, "config", "user.name", "Test")

	writeFile(t, upstream, "landed.go", "package app\n\nfunc landedUpstream() {}\n")
	runGit(t, upstream, "add", ".")
	runGit(t, upstream, "commit", "-m", "landed after clone")
	runGit(t, upstream, "checkout", "--quiet", "-b", "feature")
	writeFile(t, upstream, "app.go", "package app\n\nfunc pullRequestChange() {}\n")
	runGit(t, upstream, "commit", "--quiet", "-am", "pull request change")
	head := strings.TrimSpace(gitOutput(t, upstream, "rev-parse", "HEAD"))
	runGit(t, upstream, "update-ref", "refs/pull/7/head", head)
	runGit(t, upstream, "checkout", "--quiet", "main")
	writeFile(t, local, "app.go", "package app\n\nfunc operatorLocalEdit() {}\n")

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf(`version: 1
inference_profiles:
  finder-test:
    endpoint: %s/v1
    api_key: test-key
    model: finder-test
    protocol: chat
    max_output_tokens: 2048
roles:
  finder:
    profile: finder-test
`, server.URL)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(local); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	t.Run("unknown remote", func(t *testing.T) {
		code, err := ExecuteContext(t.Context(), []string{
			"review", "--config", configPath, "--quick", "--pr", "7", "--pr-remote", "upstream",
		}, io.Discard, io.Discard)
		if code != ExitUsage {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})

	var stdout, stderr bytes.Buffer
	code, runErr := ExecuteContext(t.Context(), []string{
		"review", "--config", configPath, "--quick", "--pr", "7", "--format", "json",
	}, &stdout, &stderr)
	if runErr != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}

	mu.Lock()
	joined := strings.Join(prompts, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "pullRequestChange") {
		t.Errorf("finder prompt lacks the pull request change: %s", joined)
	}
	for _, leaked := range []string{"operatorLocalEdit", "landedUpstream"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("finder prompt contains %s", leaked)
		}
	}

	meta := readOnlyRunMeta(t, recordDir)
	pr := meta.Scope.PullRequest
	if pr == nil || pr.Number != 7 || pr.Remote != "origin" || pr.HeadOID != head || pr.BaseRef != "origin/HEAD" {
		t.Errorf("run record pull_request = %+v", pr)
	}
	if !meta.Snapshot.Captured {
		t.Errorf("snapshot not captured from the pull request worktree: %s", meta.Snapshot.Reason)
	}
	if meta.Scope.HeadOID != head {
		t.Errorf("run record head_oid = %s, want %s", meta.Scope.HeadOID, head)
	}
	wantRepo, _ := filepath.EvalSymlinks(local)
	if gotRepo, _ := filepath.EvalSymlinks(meta.Repo); gotRepo != wantRepo {
		t.Errorf("run record repo = %s, want the operator's checkout %s", meta.Repo, local)
	}
	if list := gitOutput(t, local, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("pull request worktree left behind:\n%s", list)
	}
	if refs := strings.TrimSpace(gitOutput(t, local, "for-each-ref", "refs/roast/pull/")); refs != "" {
		t.Errorf("pull request refs left behind:\n%s", refs)
	}
	if raw, _ := os.ReadFile(filepath.Join(local, "app.go")); !strings.Contains(string(raw), "operatorLocalEdit") {
		t.Errorf("operator's checkout was modified: %q", raw)
	}
}

func readOnlyRunMeta(t *testing.T, dir string) record.Meta {
	t.Helper()
	var paths []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == record.MetaFile {
			paths = append(paths, path)
		}
		return nil
	})
	if len(paths) != 1 {
		t.Fatalf("found %d run records, want 1: %v", len(paths), paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var meta record.Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
