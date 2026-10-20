package paths

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirOverride(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", "/tmp/br-test-home")
	if got := Dir(); got != "/tmp/br-test-home" {
		t.Fatalf("Dir() = %q, want override", got)
	}
	if got := Socket(); got != filepath.Join("/tmp/br-test-home", "breakroom.sock") {
		t.Fatalf("Socket() = %q", got)
	}
}

func TestDirDefault(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", "")
	if got := Dir(); !strings.HasSuffix(got, ".breakroom") {
		t.Fatalf("Dir() = %q, want ~/.breakroom", got)
	}
}

func TestResolveWorkspace(t *testing.T) {
	t.Setenv("BREAKROOM_WORKSPACE", "/repo/root")
	if got := ResolveWorkspace("/other"); got != "/repo/root" {
		t.Fatalf("ResolveWorkspace = %q, want env override", got)
	}
	t.Setenv("BREAKROOM_WORKSPACE", "")
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveWorkspace(dir); got != want {
		t.Fatalf("ResolveWorkspace = %q, want %q", got, want)
	}
}

func TestCurrentWorkspace(t *testing.T) {
	t.Setenv("BREAKROOM_WORKSPACE", "/ws/explicit")
	got, err := CurrentWorkspace()
	if err != nil {
		t.Fatalf("CurrentWorkspace err = %v", err)
	}
	if got != "/ws/explicit" {
		t.Fatalf("CurrentWorkspace = %q, want /ws/explicit", got)
	}
}

func TestResolveWorkspaceGitRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("BREAKROOM_WORKSPACE", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "-b", "main", ".")
	wt := filepath.Join(t.TempDir(), "wt")
	run(root, "worktree", "add", wt, "-b", "side")
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveWorkspace(root); got != want {
		t.Fatalf("main ResolveWorkspace = %q, want %q", got, want)
	}
	if got := ResolveWorkspace(wt); got != want {
		t.Fatalf("worktree ResolveWorkspace = %q, want %q", got, want)
	}
	if got := GitCommonRoot(t.TempDir()); got != "" {
		t.Fatalf("GitCommonRoot(non-repo) = %q, want empty", got)
	}
	t.Setenv("BREAKROOM_WORKSPACE", "/explicit")
	if got := ResolveWorkspace(wt); got != "/explicit" {
		t.Fatalf("env override = %q, want /explicit", got)
	}
}

// A bare clone plus worktrees (a common multi-agent layout) has no ".git"
// dir: its worktrees must still share one workspace, keyed by the bare repo.
func TestResolveWorkspaceBareRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("BREAKROOM_WORKSPACE", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	run(base, "init", "-b", "main", seed)
	run(seed, "commit", "--allow-empty", "-m", "init")
	run(base, "clone", "--bare", seed, "repo.git")
	bare := filepath.Join(base, "repo.git")
	wtA, wtB := filepath.Join(base, "wt-a"), filepath.Join(base, "wt-b")
	run(bare, "worktree", "add", wtA, "-b", "a")
	run(bare, "worktree", "add", wtB, "-b", "b")
	want, err := filepath.EvalSymlinks(bare)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, dir string }{
		{"worktree a", wtA},
		{"worktree b", wtB},
		{"worktree a cleaned", filepath.Join(wtA)},
		{"bare repo", bare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveWorkspace(tc.dir); got != want {
				t.Fatalf("ResolveWorkspace(%s) = %q, want the bare repo %q", tc.dir, got, want)
			}
		})
	}
}
