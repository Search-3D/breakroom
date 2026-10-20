package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// acceptedSeries builds a finished series the human has accepted, ready to land.
func acceptedSeries(t *testing.T) (h http.Handler, ws, arch, build, wt string) {
	t.Helper()
	h, ws, arch, build = finishedSeries(t)
	doPost(t, h, "/v1/blueprints/acceptance", map[string]any{"workspace_root": ws, "name": "s1", "caller": arch})
	if code, out := doPost(t, h, "/v1/blueprints/verdict", map[string]any{"workspace_root": ws, "name": "s1", "verdict": "accept", "via": "cli"}); code != 200 {
		t.Fatalf("accept: %d %v", code, out)
	}
	takeMail(t, h, ws, arch)
	takeMail(t, h, ws, build)
	return h, ws, arch, build, blueprintField(t, h, ws, "s1", "worktree_path")
}

func TestLandRemovesWorktree(t *testing.T) {
	h, ws, _, build, wt := acceptedSeries(t)
	code, out := doPost(t, h, "/v1/blueprints/land", map[string]any{
		"workspace_root": ws, "name": "s1", "method": "branch", "ref": "breakroom/s1", "via": "cli",
	})
	if code != 200 || out["status"] != "handover" {
		t.Fatalf("land: %d %v", code, out)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if !strings.Contains(gitOut(t, ws, "branch", "--list", "breakroom/s1"), "breakroom/s1") {
		t.Fatal("branch removed")
	}
	if got := agentWorktree(t, h, ws, build); got != ws {
		t.Fatalf("builder worktree = %q, want back to %q", got, ws)
	}
	// The builder gets the facts; "what's next?" is the human's question.
	if mail := takeMail(t, h, ws, build); !mailContaining(mail, "breakroom/s1") || mailContaining(mail, "next series") {
		t.Fatalf("builder landing mail = %v", mail)
	}
	if hd, _ := out["handoff"].(string); !strings.Contains(hd, "breakroom/s1") || !strings.Contains(hd, "next series") {
		t.Fatalf("handoff = %q", hd)
	}
}

func TestLandDirtyRefuses(t *testing.T) {
	h, ws, _, _, wt := acceptedSeries(t)
	writeFile(t, wt, "stray.txt", "x\n")
	code, out := doPost(t, h, "/v1/blueprints/land", map[string]any{
		"workspace_root": ws, "name": "s1", "method": "branch", "ref": "breakroom/s1", "via": "cli",
	})
	wantErr(t, code, out, "worktree_dirty")
	if got := blueprintField(t, h, ws, "s1", "status"); got != "accepted" {
		t.Fatalf("status = %q, want accepted", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "stray.txt")); err != nil {
		t.Fatal("dirty worktree touched")
	}
}

func TestLandNonSeries(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws/a", "architect")
	build := register(t, h, "/ws", "/ws/b", "builder")
	doPost(t, h, "/v1/blueprints/init", map[string]any{"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build})
	approveRoadmap(t, h, "/ws", "bp", arch, "one")
	spec(t, h, "/ws", "bp", arch, 1, "one")
	submit(t, h, "/ws", "bp", build, 1)
	signoff(t, h, "/ws", "bp", arch, 1)
	doPost(t, h, "/v1/blueprints/acceptance", map[string]any{"workspace_root": "/ws", "name": "bp", "caller": arch})
	doPost(t, h, "/v1/blueprints/verdict", map[string]any{"workspace_root": "/ws", "name": "bp", "verdict": "accept", "via": "cli"})
	code, out := doPost(t, h, "/v1/blueprints/land", map[string]any{
		"workspace_root": "/ws", "name": "bp", "method": "pr", "ref": "https://example.test/pr/1", "via": "cli",
	})
	if code != 200 || out["status"] != "handover" {
		t.Fatalf("land: %d %v", code, out)
	}
}

func TestCloseSeriesAbandon(t *testing.T) {
	ws := seriesRepo(t)
	h := newTestServer(t).Handler()
	arch := register(t, h, ws, ws, "architect")
	initSeries(t, h, ws, "s1", arch)
	proposeApprove(t, h, ws, "s1", arch, "", "one")
	wt := blueprintField(t, h, ws, "s1", "worktree_path")
	code, out := doPost(t, h, "/v1/blueprints/close", map[string]any{"workspace_root": ws, "name": "s1", "caller": arch})
	if code != 200 {
		t.Fatalf("close: %d %v", code, out)
	}
	hd, _ := out["handoff"].(string)
	if !strings.Contains(hd, ".workspaces/s1") || !strings.Contains(hd, "git worktree remove") {
		t.Fatalf("abandon handoff = %q", hd)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("abandoned worktree was removed")
	}
}

func TestLandRemoveFailsHonestHandoff(t *testing.T) {
	h, ws, _, _, wt := acceptedSeries(t)
	sh(t, ws, "worktree", "lock", wt)
	code, out := doPost(t, h, "/v1/blueprints/land", map[string]any{
		"workspace_root": ws, "name": "s1", "method": "branch", "ref": "breakroom/s1", "via": "cli",
	})
	if code != 200 || out["warning"] == nil {
		t.Fatalf("land: %d %v, want a removal warning", code, out)
	}
	if hd, _ := out["handoff"].(string); strings.Contains(hd, "has been removed") || !strings.Contains(hd, "git worktree remove") {
		t.Fatalf("handoff claims removal or gives no next step: %q", hd)
	}
}

// A merge landing is checked: the series branch must be in the main
// checkout's HEAD before the worktree goes.
func TestLandMergeVerified(t *testing.T) {
	h, ws, _, _, wt := acceptedSeries(t)
	writeFile(t, wt, "feature.txt", "x\n")
	sh(t, wt, "add", ".")
	sh(t, wt, "commit", "-m", "feature")
	land := map[string]any{"workspace_root": ws, "name": "s1", "method": "merge", "via": "cli"}
	code, out := doPost(t, h, "/v1/blueprints/land", land)
	wantErr(t, code, out, "not_merged")
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("worktree removed before the merge")
	}
	sh(t, ws, "merge", "--no-edit", "breakroom/s1")
	if code, out := doPost(t, h, "/v1/blueprints/land", land); code != 200 || out["status"] != "handover" {
		t.Fatalf("land after merge: %d %v", code, out)
	}
}

// A pr landing records where the PR is.
func TestLandPRNeedsRef(t *testing.T) {
	h, ws, _, _, _ := acceptedSeries(t)
	code, out := doPost(t, h, "/v1/blueprints/land", map[string]any{"workspace_root": ws, "name": "s1", "method": "pr", "via": "cli"})
	wantErr(t, code, out, "bad_request")
}

func TestLandInvalidMethod(t *testing.T) {
	h, ws, _, _, _ := acceptedSeries(t)
	code, out := doPost(t, h, "/v1/blueprints/land", map[string]any{"workspace_root": ws, "name": "s1", "method": "invalid", "via": "cli"})
	wantErr(t, code, out, "bad_request")
}

func TestCloseFromAcceptedNotifiesBuilderAndResetsWorktree(t *testing.T) {
	h, ws, arch, build, wt := acceptedSeries(t)
	if got := agentWorktree(t, h, ws, build); got != wt {
		t.Fatalf("builder worktree before close = %q, want %q", got, wt)
	}

	code, out := doPost(t, h, "/v1/blueprints/close", map[string]any{
		"workspace_root": ws, "name": "s1", "caller": arch,
	})
	if code != 200 || out["status"] != "handover" {
		t.Fatalf("close: %d %v", code, out)
	}
	handoff, _ := out["handoff"].(string)
	if handoff == "" || !strings.Contains(handoff, "without landing") {
		t.Fatalf("expected handoff with unlanded details, got %q", handoff)
	}

	if got := agentWorktree(t, h, ws, build); got != ws {
		t.Fatalf("builder worktree after close = %q, want %q", got, ws)
	}

	mail := takeMail(t, h, ws, build)
	if len(mail) == 0 {
		t.Fatal("builder received no mail on close from accepted")
	}
	content, _ := mail[0]["content"].(string)
	if !strings.Contains(content, "HANDOVER") || !strings.Contains(content, "Closed without landing") {
		t.Fatalf("builder mail content = %q, want HANDOVER notification", content)
	}
}

func TestLandManualKeepsWorktreeAndDisconnectsAgents(t *testing.T) {
	h, ws, arch, build, wt := acceptedSeries(t)
	writeFile(t, wt, "uncommitted.txt", "keep me\n")
	code, out := doPost(t, h, "/v1/blueprints/land", map[string]any{
		"workspace_root": ws, "name": "s1", "method": "manual", "via": "cli",
	})
	if code != 200 || out["status"] != "handover" {
		t.Fatalf("manual land: %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(wt, "uncommitted.txt")); err != nil {
		t.Fatalf("worktree removed on manual land: %v", err)
	}
	if !strings.Contains(gitOut(t, ws, "branch", "--list", "breakroom/s1"), "breakroom/s1") {
		t.Fatal("branch removed on manual land")
	}
	if hd, _ := out["handoff"].(string); !strings.Contains(hd, ".workspaces/s1") || !strings.Contains(hd, "disconnected") {
		t.Fatalf("manual land handoff = %q", hd)
	}
	if live := listAgents(t, h, ws); len(live) != 0 {
		t.Fatalf("live agents after manual land = %v, want both disconnected", live)
	}
	if mail := takeMail(t, h, ws, build); !mailContaining(mail, "disconnected") {
		t.Fatalf("builder mail after manual land = %v", mail)
	}
	_ = arch
}
