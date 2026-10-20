package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv skips without git and isolates git from the user's config.
func testEnv(t *testing.T) {
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

// git runs a git command in dir and returns its output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// initRepo creates an empty git repo in a temp dir.
func initRepo(t *testing.T) string {
	t.Helper()
	testEnv(t)
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main", ".")
	return dir
}

// write writes a file under dir, creating folders as needed.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStatus(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "f.txt", "a\n")
	write(t, dir, "g.txt", "b\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "init")
	write(t, dir, "f.txt", "a\nchanged\n") // unstaged modification
	write(t, dir, "new.txt", "n\n")
	git(t, dir, "add", "new.txt") // staged new file
	write(t, dir, "untracked.txt", "u\n")
	write(t, dir, "node_modules/pkg/index.d.ts", "export {}\n")
	write(t, dir, "dist/bundle.js", "console.log(1)\n")
	write(t, dir, "internal/build/new.go", "package build\n") // source, not output
	git(t, dir, "mv", "g.txt", "renamed.txt")
	got, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"f.txt": " M", "new.txt": "A ", "untracked.txt": "??", "renamed.txt": "R ", "internal/build/new.go": "??"}
	if len(got) != len(want) {
		t.Fatalf("status = %+v, want %d entries", got, len(want))
	}
	for _, f := range got {
		if want[f.Path] != f.Status {
			t.Fatalf("status = %+v, want %v", got, want)
		}
	}
}

func TestStatusNotARepo(t *testing.T) {
	testEnv(t)
	if _, err := Status(context.Background(), t.TempDir()); !errors.Is(err, ErrNotARepo) {
		t.Fatalf("err = %v, want ErrNotARepo", err)
	}
}

func TestDiff(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "f.txt", "a\nb\n")
	write(t, dir, "g.txt", "x\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "init")
	write(t, dir, "f.txt", "a\nB\n")
	write(t, dir, "new.txt", "hello\n")
	ctx := context.Background()

	res, err := Diff(ctx, dir, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Diff, "-b\n+B") || strings.Contains(res.Diff, "new.txt") {
		t.Fatalf("diff = %q, want f.txt hunk without untracked", res.Diff)
	}
	if len(res.Files) != 1 || res.Files[0] != "f.txt" {
		t.Fatalf("files = %v", res.Files)
	}

	res, err = Diff(ctx, dir, DiffOptions{Paths: []string{"g.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Diff != "" || len(res.Files) != 0 {
		t.Fatalf("filtered diff = %q %v, want empty", res.Diff, res.Files)
	}

	res, err = Diff(ctx, dir, DiffOptions{IncludeUntracked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Diff, "new file") || !strings.Contains(res.Diff, "+hello") {
		t.Fatalf("untracked diff = %q", res.Diff)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files = %v, want [f.txt new.txt]", res.Files)
	}
}

func TestDiffBinary(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "f.txt", "a\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0, 1, 2, 0, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Diff(context.Background(), dir, DiffOptions{IncludeUntracked: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Diff, "Binary files /dev/null and b/bin.dat differ") {
		t.Fatalf("diff = %q, want binary marker", res.Diff)
	}
}

func TestDiffTruncate(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "f.txt", "a\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "init")
	write(t, dir, "f.txt", strings.Repeat("0123456789\n", 100))
	res, err := Diff(context.Background(), dir, DiffOptions{MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || !strings.Contains(res.Diff, "[truncated") {
		t.Fatalf("truncated=%v diff=%q", res.Truncated, res.Diff)
	}
	// The marker fits inside the cap: the store rejects anything larger.
	if len(res.Diff) > 100 {
		t.Fatalf("len(diff) = %d, want <= 100", len(res.Diff))
	}
	if len(res.Files) != 1 {
		t.Fatalf("files = %v, want names despite truncation", res.Files)
	}
}

func TestDiffNoHead(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "staged.txt", "s\n")
	git(t, dir, "add", "staged.txt")
	write(t, dir, "untracked.txt", "u\n")
	res, err := Diff(context.Background(), dir, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Diff, "a/staged.txt") || strings.Contains(res.Diff, "untracked") {
		t.Fatalf("diff = %q, want staged only", res.Diff)
	}
	res, err = Diff(context.Background(), dir, DiffOptions{IncludeUntracked: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || !strings.Contains(res.Diff, "+u") {
		t.Fatalf("diff = %q %v", res.Diff, res.Files)
	}
}

func TestCheckVersion(t *testing.T) {
	testEnv(t)
	if err := CheckVersion(context.Background()); err != nil {
		t.Fatalf("CheckVersion = %v", err)
	}
}

// Agents may register a repo subdirectory as their worktree. Paths are
// repo-root relative everywhere (status, diff, filters), so untracked
// content must still be read from the right place.
func TestDiffFromSubdir(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "sub/a.txt", "a\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "init")
	write(t, dir, "sub/a.txt", "changed\n")
	write(t, dir, "sub/new.txt", "hello\n")
	write(t, dir, "top.txt", "root\n")
	sub := filepath.Join(dir, "sub")
	res, err := Diff(context.Background(), sub, DiffOptions{IncludeUntracked: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+changed", "+++ b/sub/new.txt", "+hello", "+root"} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(res.Diff, want) {
				t.Fatalf("diff lacks %q:\n%s", want, res.Diff)
			}
		})
	}
	res, err = Diff(context.Background(), sub, DiffOptions{Paths: []string{"sub/new.txt"}, IncludeUntracked: true})
	if err != nil || len(res.Files) != 1 || !strings.Contains(res.Diff, "+hello") {
		t.Fatalf("root-relative filter: files=%v err=%v diff=%q", res.Files, err, res.Diff)
	}
}
