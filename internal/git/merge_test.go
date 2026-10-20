package git

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// linkedWorktree inits a repo with f.txt committed and adds a second
// worktree of the same repo.
func linkedWorktree(t *testing.T) (main, wt string) {
	t.Helper()
	main = initRepo(t)
	write(t, main, "f.txt", "a\nb\nc\nd\ne\nf\n")
	git(t, main, "add", "-A")
	git(t, main, "commit", "-m", "init")
	wt = filepath.Join(t.TempDir(), "wt")
	git(t, main, "worktree", "add", wt, "-b", "side")
	return main, wt
}

func TestConflictsDirtyWorktrees(t *testing.T) {
	main, wt := linkedWorktree(t)
	write(t, main, "f.txt", "a\nB-OURS\nc\nd\ne\nf\n")
	write(t, wt, "f.txt", "a\nB-THEIRS\nc\nd\ne\nf\n")
	res, err := Conflicts(context.Background(), main, wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Path != "f.txt" {
		t.Fatalf("conflicts = %+v", res.Conflicts)
	}
	c := res.Conflicts[0]
	if c.OursLines == nil || *c.OursLines != "2" {
		t.Fatalf("ours_lines = %v, want 2", c.OursLines)
	}
	if c.TheirsLines == nil || *c.TheirsLines != "2" {
		t.Fatalf("theirs_lines = %v, want 2", c.TheirsLines)
	}
	if len(res.OverlapOnly) != 0 {
		t.Fatalf("overlap = %v, want none", res.OverlapOnly)
	}
}

func TestConflictsOverlapClean(t *testing.T) {
	main, wt := linkedWorktree(t)
	write(t, main, "f.txt", "a\nB\nc\nd\ne\nf\n")
	write(t, wt, "f.txt", "a\nb\nc\nd\nE\nf\n")
	res, err := Conflicts(context.Background(), main, wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", res.Conflicts)
	}
	if len(res.OverlapOnly) != 1 || res.OverlapOnly[0] != "f.txt" {
		t.Fatalf("overlap = %v, want [f.txt]", res.OverlapOnly)
	}
}

func TestConflictsUntracked(t *testing.T) {
	main, wt := linkedWorktree(t)
	write(t, main, "new.txt", "ours\n")
	write(t, wt, "new.txt", "theirs\n")
	res, err := Conflicts(context.Background(), main, wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Path != "new.txt" {
		t.Fatalf("conflicts = %+v, want add/add on new.txt", res.Conflicts)
	}
	if c := res.Conflicts[0]; c.OursLines != nil || c.TheirsLines != nil {
		t.Fatalf("lines = %v/%v, want nil (no base)", c.OursLines, c.TheirsLines)
	}
	// Identical bytes merge cleanly.
	write(t, wt, "new.txt", "ours\n")
	res, err = Conflicts(context.Background(), main, wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", res.Conflicts)
	}
	if len(res.OverlapOnly) != 1 || res.OverlapOnly[0] != "new.txt" {
		t.Fatalf("overlap = %v, want [new.txt]", res.OverlapOnly)
	}
}

func TestConflictsNoCommits(t *testing.T) {
	dir := initRepo(t)
	if _, err := Conflicts(context.Background(), dir, dir); !errors.Is(err, ErrNoCommits) {
		t.Fatalf("err = %v, want ErrNoCommits", err)
	}
}

func TestConflictsSeparateClones(t *testing.T) {
	a := initRepo(t)
	write(t, a, "f.txt", "a\n")
	git(t, a, "add", "-A")
	git(t, a, "commit", "-m", "init")
	b := initRepo(t)
	write(t, b, "f.txt", "different\n")
	git(t, b, "add", "-A")
	git(t, b, "commit", "-m", "init")
	if _, err := Conflicts(context.Background(), a, b); !errors.Is(err, ErrCannotCompare) {
		t.Fatalf("err = %v, want ErrCannotCompare", err)
	}
}

// Subdirectory worktrees: identical untracked files must still compare as
// identical (bytes read from the repo root, not the subdirectory).
func TestConflictsFromSubdirs(t *testing.T) {
	main, wt := linkedWorktree(t)
	write(t, main, "sub/new.txt", "same\n")
	write(t, wt, "sub/new.txt", "same\n")
	res, err := Conflicts(context.Background(), filepath.Join(main, "sub"), filepath.Join(wt, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 || len(res.OverlapOnly) != 1 || res.OverlapOnly[0] != "sub/new.txt" {
		t.Fatalf("conflicts = %+v overlap = %v, want overlap-only sub/new.txt", res.Conflicts, res.OverlapOnly)
	}
}

func TestConflictsLeavesNoObjects(t *testing.T) {
	main, wt := linkedWorktree(t)
	write(t, main, "f.txt", "a\nB-OURS\nc\nd\ne\nf\n")
	write(t, wt, "f.txt", "a\nB-THEIRS\nc\nd\ne\nf\n")
	before := git(t, main, "count-objects")
	if _, err := Conflicts(context.Background(), main, wt); err != nil {
		t.Fatal(err)
	}
	if after := git(t, main, "count-objects"); after != before {
		t.Fatalf("objects written to repo: %q -> %q", before, after)
	}
}
