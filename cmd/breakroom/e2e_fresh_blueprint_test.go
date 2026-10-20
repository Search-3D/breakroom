package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EBlueprintFreshNonGit exercises a full Architect + Builder series on a fresh,
// non-git directory to verify auto git init, worktree creation, spec/submit/signoff,
// acceptance, and landing.
func TestE2EBlueprintFreshNonGit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e blueprint test in short mode")
	}
	e := twoAgents(t)

	// Ensure e.ws is NOT a git repo initially.
	if _, err := os.Stat(filepath.Join(e.ws, ".git")); !os.IsNotExist(err) {
		t.Fatalf("expected ws to have no .git, got err %v", err)
	}

	// 1. Architect initializes a series.
	initData := wantOK(t, e.a.tool("blueprint_init", map[string]any{
		"name": "fresh-series",
		"goal": "validate fresh auto-git-init series",
	}))
	if s, _ := initData["status"].(string); s != "drafting" {
		t.Fatalf("blueprint_init status = %v, want drafting", s)
	}

	// 2. Builder joins series.
	joinData := wantOK(t, e.b.tool("blueprint_join", map[string]any{"name": "fresh-series"}))
	if j, _ := joinData["joined"].(bool); !j {
		t.Fatalf("blueprint_join failed: %v", joinData)
	}

	// 3. Architect submits roadmap.
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{
		"name": "fresh-series",
		"phases": []any{
			map[string]any{"phase": 1, "title": "implement proof"},
		},
	}))

	// 4. Human/CLI approves the roadmap.
	// This must auto-init git repository and create the series worktree.
	out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "approve", "fresh-series")
	if code != 0 {
		t.Fatalf("blueprint approve failed (exit %d): %s", code, out)
	}

	// Verify .git now exists in e.ws.
	if _, err := os.Stat(filepath.Join(e.ws, ".git")); err != nil {
		t.Fatalf("expected auto git init in %s, got error: %v", e.ws, err)
	}

	// Check blueprint status over MCP.
	st := wantOK(t, e.a.tool("blueprint_status", map[string]any{"name": "fresh-series"}))
	bp, _ := st["blueprint"].(map[string]any)
	wtPath, _ := bp["worktree_path"].(string)
	expectedWt := filepath.Join(e.ws, ".workspaces", "fresh-series")
	evalExpected, err := filepath.EvalSymlinks(expectedWt)
	if err != nil {
		evalExpected = expectedWt
	}
	evalGot, err := filepath.EvalSymlinks(wtPath)
	if err != nil {
		evalGot = wtPath
	}
	if evalGot != evalExpected {
		t.Fatalf("worktree_path = %q, want %q", evalGot, evalExpected)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("expected worktree to exist at %s: %v", wtPath, err)
	}

	// 5. Architect specs Phase 1.
	wantOK(t, e.a.tool("blueprint_spec", map[string]any{
		"name":         "fresh-series",
		"phase":        1,
		"title":        "implement proof",
		"objective":    "create proof in worktree",
		"requirements": []any{map[string]any{"id": "r1", "text": "deliver proof artifact"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "proof is accepted"}},
	}))

	// 6. Builder submits Phase 1.
	wantOK(t, e.b.tool("blueprint_submit", map[string]any{
		"name":  "fresh-series",
		"phase": 1,
		"proof": "tests passed cleanly",
		"items": []any{
			map[string]any{"requirement_id": "r1", "summary": "proof delivered"},
		},
	}))

	// 7. Architect inspects and signs off Phase 1.
	wantOK(t, e.a.tool("blueprint_inspect", map[string]any{
		"name":           "fresh-series",
		"phase":          1,
		"verdict":        "signoff",
		"acceptance_ids": []any{"a1"},
	}))

	// 8. Architect requests human acceptance.
	reqData := wantOK(t, e.a.tool("blueprint_request_acceptance", map[string]any{
		"name": "fresh-series",
	}))
	handoff, _ := reqData["handoff"].(string)
	if !strings.Contains(handoff, "Reply accept") {
		t.Fatalf("unexpected handoff: %s", handoff)
	}

	// 9. Human accepts via CLI.
	out, code = runIn(t, e.home, e.bin, e.ws, "blueprint", "accept", "fresh-series")
	if code != 0 {
		t.Fatalf("blueprint accept failed (exit %d): %s", code, out)
	}

	// 10. Human lands via CLI (manual method).
	out, code = runIn(t, e.home, e.bin, e.ws, "blueprint", "land", "fresh-series", "--method", "manual")
	if code != 0 {
		t.Fatalf("blueprint land failed (exit %d): %s", code, out)
	}

	// 11. Final status check.
	finalSt := wantOK(t, e.a.tool("blueprint_status", map[string]any{"name": "fresh-series"}))
	finalBp, _ := finalSt["blueprint"].(map[string]any)
	if s, _ := finalBp["status"].(string); s != "handover" {
		t.Fatalf("final status = %v, want handover", s)
	}
}
