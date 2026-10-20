package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWorktreeAddRemove(t *testing.T) {
	repo := initRepo(t)
	write(t, repo, "a.txt", "one\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "init")
	ctx := context.Background()
	head, err := Head(ctx, repo)
	if err != nil || len(head) != 40 {
		t.Fatalf("head = %q (%v)", head, err)
	}
	if dirty, err := Dirty(ctx, repo); err != nil || dirty {
		t.Fatalf("clean repo dirty = %v (%v)", dirty, err)
	}
	wt := repo + "-s1"
	if err := WorktreeAdd(ctx, repo, wt, "breakroom/s1", head); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "a.txt")); err != nil {
		t.Fatalf("worktree missing checkout: %v", err)
	}
	if out := git(t, repo, "branch", "--list", "breakroom/s1"); !strings.Contains(out, "breakroom/s1") {
		t.Fatal("branch not created")
	}
	write(t, wt, "new.txt", "x\n")
	if err := WorktreeRemove(ctx, repo, wt); !errors.Is(err, ErrWorktreeDirty) {
		t.Fatalf("dirty remove err = %v, want ErrWorktreeDirty", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("dirty worktree was removed")
	}
	if err := os.Remove(filepath.Join(wt, "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := WorktreeRemove(ctx, repo, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if out := git(t, repo, "branch", "--list", "breakroom/s1"); !strings.Contains(out, "breakroom/s1") {
		t.Fatal("branch deleted with the worktree")
	}
	write(t, repo, "a.txt", "two\n")
	if dirty, err := Dirty(ctx, repo); err != nil || !dirty {
		t.Fatalf("edited repo dirty = %v (%v)", dirty, err)
	}
}

// Initializing stages and commits everything under dir, so the home
// directory (a workspace from a host that starts MCP in $HOME) is refused.
func TestEnsureHeadRefusesHome(t *testing.T) {
	testEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, _, err := EnsureHead(context.Background(), home); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("EnsureHead(home) err = %v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".git")); !os.IsNotExist(err) {
		t.Fatalf("home .git exists after refusal: %v", err)
	}
}

func TestEnsureHeadStagesExistingFiles(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"test"}`+"\n")
	write(t, dir, "src/index.ts", "console.log(42);\n")
	ctx := context.Background()
	head, initial, err := EnsureHead(ctx, dir)
	if err != nil {
		t.Fatalf("EnsureHead: %v", err)
	}
	if initial != 2 {
		t.Errorf("initial = %d, want 2", initial)
	}
	wt := filepath.Join(dir, ".workspaces", "s1")
	if err := WorktreeAdd(ctx, dir, wt, "breakroom/s1", head); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "package.json")); err != nil {
		t.Errorf("worktree missing package.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "src", "index.ts")); err != nil {
		t.Errorf("worktree missing src/index.ts: %v", err)
	}
	dirty, err := Dirty(ctx, dir)
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if dirty {
		t.Errorf("repo should not be dirty after EnsureHead commits existing files")
	}
}

// The automatic initial commit leaves dependency folders and secrets out.
func TestEnsureHeadSkipsNoiseAndSecrets(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	write(t, dir, "main.go", "package main\n")
	write(t, dir, "node_modules/x/index.js", "x\n")
	write(t, dir, "web/node_modules/y.js", "y\n")
	write(t, dir, "dist/out.js", "z\n")
	write(t, dir, "cmd/dist/keep.go", "package dist\n")
	write(t, dir, ".env", "SECRET=1\n")
	write(t, dir, "certs/server.key", "k\n")
	ctx := context.Background()
	_, initial, err := EnsureHead(ctx, dir)
	if err != nil {
		t.Fatalf("EnsureHead: %v", err)
	}
	out, err := run(ctx, dir, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(out); initial != 2 || !slices.Equal(got, []string{"cmd/dist/keep.go", "main.go"}) {
		t.Fatalf("initial = %d, committed %v", initial, got)
	}
}

// A bare repository can host series worktrees but can't be auto-initialized.
func TestBareRepo(t *testing.T) {
	testEnv(t)
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repo.git")
	if _, err := run(ctx, filepath.Dir(bare), "init", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	if !IsBare(ctx, bare) {
		t.Fatal("IsBare = false")
	}
	if _, _, err := EnsureHead(ctx, bare); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("EnsureHead(empty bare) err = %v", err)
	}
	if IsBare(ctx, t.TempDir()) {
		t.Fatal("IsBare(non-repo) = true")
	}
}
