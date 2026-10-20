package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv skips without git and isolates git from the user's config.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
}

// sh runs a git command in dir, failing the test on error.
func sh(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo creates an empty git repo in a temp dir.
func initRepo(t *testing.T) string {
	t.Helper()
	gitEnv(t)
	dir := t.TempDir()
	sh(t, dir, "init", "-b", "main", ".")
	return dir
}

// writeFile writes a file under dir, creating folders as needed.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiffEndpoint(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", "a\nb\n")
	sh(t, repo, "add", "-A")
	sh(t, repo, "commit", "-m", "init")
	writeFile(t, repo, "f.txt", "a\nB\n")
	h := newTestServer(t).Handler()
	a1 := register(t, h, repo, repo, "backend")

	code, out := doPost(t, h, "/v1/git/diff", map[string]any{
		"workspace_root": repo, "agent_id": a1,
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if diff, _ := out["diff"].(string); !strings.Contains(diff, "-b\n+B") {
		t.Fatalf("diff = %q", diff)
	}
	if files, _ := out["files"].([]any); len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if out["truncated"] != false {
		t.Fatalf("out = %v, want truncated=false", out)
	}

	code, out = doPost(t, h, "/v1/git/diff", map[string]any{
		"workspace_root": repo, "agent_id": "agent-9",
	})
	wantErr(t, code, out, "unknown_agent")

	code, out = doPost(t, h, "/v1/git/diff", map[string]any{
		"workspace_root": repo, "agent_id": a1, "summarize": true,
	})
	if code != 200 || out["fallback"] != true {
		t.Fatalf("code=%d out=%v, want fallback", code, out)
	}

	plain := t.TempDir()
	a2 := register(t, h, plain, plain, "docs")
	code, out = doPost(t, h, "/v1/git/diff", map[string]any{
		"workspace_root": plain, "agent_id": a2,
	})
	wantErr(t, code, out, "not_a_repo")
}

func TestConflictsEndpoint(t *testing.T) {
	root := t.TempDir()
	gitEnv(t)
	main := filepath.Join(root, "main")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, main, "init", "-b", "main", ".")
	writeFile(t, main, "f.txt", "a\nb\nc\nd\ne\nf\n")
	sh(t, main, "add", "-A")
	sh(t, main, "commit", "-m", "init")
	wt := filepath.Join(root, "wt")
	sh(t, main, "worktree", "add", wt, "-b", "side")
	writeFile(t, main, "f.txt", "a\nB-OURS\nc\nd\ne\nf\n")
	writeFile(t, wt, "f.txt", "a\nB-THEIRS\nc\nd\ne\nf\n")

	h := newTestServer(t).Handler()
	a1 := register(t, h, root, main, "backend")
	a2 := register(t, h, root, wt, "frontend")
	code, out := doPost(t, h, "/v1/git/conflicts", map[string]any{
		"workspace_root": root, "agent_id": a1, "target_agent_id": a2,
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	conflicts, _ := out["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %v", conflicts)
	}
	c := conflicts[0].(map[string]any)
	if c["path"] != "f.txt" || c["ours_lines"] != "2" || c["theirs_lines"] != "2" {
		t.Fatalf("conflict = %v", c)
	}

	code, out = doPost(t, h, "/v1/git/conflicts", map[string]any{
		"workspace_root": root, "agent_id": a1, "target_agent_id": "agent-9",
	})
	wantErr(t, code, out, "unknown_agent")
}

func TestUpdateRefreshesFiles(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", "a\n")
	sh(t, repo, "add", "-A")
	sh(t, repo, "commit", "-m", "init")
	writeFile(t, repo, "f.txt", "a\nchanged\n")
	h := newTestServer(t).Handler()
	a1 := register(t, h, repo, repo, "backend")

	code, out := doPost(t, h, "/v1/agents/update", map[string]any{
		"workspace_root": repo, "agent_id": a1, "status": "active",
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	agent, _ := out["agent"].(map[string]any)
	if files, _ := agent["modified_files"].([]any); len(files) != 1 {
		t.Fatalf("modified_files = %v, want [f.txt] from git status", files)
	}

	// A second agent on the same file triggers an overlap warning.
	a2 := register(t, h, repo, repo, "frontend")
	code, out = doPost(t, h, "/v1/agents/update", map[string]any{
		"workspace_root": repo, "agent_id": a2, "status": "active",
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	warns, _ := out["warnings"].([]any)
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1 overlap", warns)
	}

	// Non-repo worktrees stay silent.
	plain := t.TempDir()
	a3 := register(t, h, plain, plain, "docs")
	code, out = doPost(t, h, "/v1/agents/update", map[string]any{
		"workspace_root": plain, "agent_id": a3, "status": "active",
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if warns, _ := out["warnings"].([]any); len(warns) != 0 {
		t.Fatalf("warnings = %v, want none", warns)
	}
}
