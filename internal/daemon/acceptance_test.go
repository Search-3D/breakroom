package daemon

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// submit has the builder deliver a phase.
func submit(t *testing.T, h http.Handler, ws, name, build string, phase int) {
	t.Helper()
	code, out := doPost(t, h, "/v1/blueprints/submit", map[string]any{
		"workspace_root": ws, "name": name, "caller": build, "phase": phase,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "done"}}, "proof": "ok",
	})
	if code != 200 {
		t.Fatalf("submit: %d %v", code, out)
	}
}

// signoff has the architect sign off a phase.
func signoff(t *testing.T, h http.Handler, ws, name, arch string, phase int) {
	t.Helper()
	code, out := doPost(t, h, "/v1/blueprints/inspect", map[string]any{
		"workspace_root": ws, "name": name, "caller": arch, "phase": phase,
		"verdict": "signoff", "acceptance_ids": []any{"a1"},
	})
	if code != 200 {
		t.Fatalf("inspect: %d %v", code, out)
	}
}

// finishedSeries runs a 1-phase series in a real repo through sign-off.
func finishedSeries(t *testing.T) (h http.Handler, ws, arch, build string) {
	t.Helper()
	ws = seriesRepo(t)
	h = newTestServer(t).Handler()
	// The architect's host PID lets its stop hook (archHostPID in
	// ancestors) be told apart from the builder's in the shared root.
	_, out := doPost(t, h, "/v1/agents/register", map[string]any{
		"workspace_root": ws, "worktree_path": ws, "role": "architect", "host_pid": archHostPID,
	})
	arch = out["agent_id"].(string)
	build = register(t, h, ws, ws, "builder")
	initSeries(t, h, ws, "s1", arch)
	join(t, h, ws, build, "s1")
	proposeApprove(t, h, ws, "s1", arch, "", "one")
	spec(t, h, ws, "s1", arch, 1, "one")
	submit(t, h, ws, "s1", build, 1)
	signoff(t, h, ws, "s1", arch, 1)
	takeMail(t, h, ws, arch)
	takeMail(t, h, ws, build)
	return h, ws, arch, build
}

const archHostPID = 4242

// gateKind runs a short stop gate for the agent in wt and returns its decision.
func gateKind(t *testing.T, h http.Handler, ws, wt string, ancestors ...int64) map[string]any {
	t.Helper()
	code, out := doPost(t, h, "/v1/gate/stop", map[string]any{"workspace_root": ws, "worktree_path": wt, "hold_sec": 1, "ancestors": ancestors})
	if code != 200 {
		t.Fatalf("gate: %d %v", code, out)
	}
	return out
}

func TestAcceptanceRoutes(t *testing.T) {
	h, ws, arch, build := finishedSeries(t)
	if out := gateKind(t, h, ws, ws, archHostPID); out["kind"] != "reminder" || !strings.Contains(out["reason"].(string), "blueprint_request_acceptance") {
		t.Fatalf("architect gate after the last sign-off = %v", out)
	}
	code, out := doPost(t, h, "/v1/blueprints/acceptance", map[string]any{"workspace_root": ws, "name": "s1", "caller": build})
	wantErr(t, code, out, "forbidden_role")
	code, out = doPost(t, h, "/v1/blueprints/acceptance", map[string]any{"workspace_root": ws, "name": "s1", "caller": arch})
	handoff, _ := out["handoff"].(string)
	if code != 200 || out["status"] != "acceptance" || !strings.Contains(handoff, ".workspaces/s1") || !strings.Contains(handoff, "Reply accept") {
		t.Fatalf("request: %d %v", code, out)
	}
	if out := gateKind(t, h, ws, ws, archHostPID); out["action"] != "allow" {
		t.Fatalf("gate while the human reviews = %v, want allow", out)
	}
	code, out = doPost(t, h, "/v1/blueprints/verdict", map[string]any{"workspace_root": ws, "name": "s1", "verdict": "accept", "via": "cli"})
	handoff, _ = out["handoff"].(string)
	if code != 200 || out["status"] != "accepted" {
		t.Fatalf("accept: %d %v", code, out)
	}
	for _, want := range []string{"1.", "2.", "3.", "4.", "Merge into your main checkout", "push", "pull request", "disconnect the agents"} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(handoff, want) {
				t.Fatalf("landing handoff lacks %q: %q", want, handoff)
			}
		})
	}
	if !mailContaining(takeMail(t, h, ws, arch), "accepted") {
		t.Fatal("architect not told of the CLI acceptance")
	}
	if mail := takeMail(t, h, ws, build); len(mail) != 0 {
		t.Fatalf("builder mail on accept = %v, want none until land or close", mail)
	}
}

// The builder parks through the human's review and acceptance until landing
// (or a follow-up spec) wakes it.
func TestBuilderHeldThroughAcceptance(t *testing.T) {
	h, ws, arch, build := finishedSeries(t)
	bwt := filepath.Join(ws, ".workspaces", "s1")
	doPost(t, h, "/v1/blueprints/acceptance", map[string]any{"workspace_root": ws, "name": "s1", "caller": arch})
	if out := gateKind(t, h, ws, bwt); out["agent"] != build || out["kind"] != "hold_expired" {
		t.Fatalf("builder gate while the human reviews = %v", out)
	}
	doPost(t, h, "/v1/blueprints/verdict", map[string]any{"workspace_root": ws, "name": "s1", "verdict": "accept", "via": "cli"})
	if out := gateKind(t, h, ws, bwt); out["agent"] != build || out["kind"] != "hold_expired" {
		t.Fatalf("builder gate after accept (before land) = %v", out)
	}
	doPost(t, h, "/v1/blueprints/land", map[string]any{"workspace_root": ws, "name": "s1", "method": "branch", "ref": "breakroom/s1", "via": "cli"})
	if !mailContaining(takeMail(t, h, ws, build), "LANDED") {
		t.Fatal("builder not mailed on land")
	}
}

func TestVerdictChangesRoute(t *testing.T) {
	h, ws, arch, build := finishedSeries(t)
	doPost(t, h, "/v1/blueprints/acceptance", map[string]any{"workspace_root": ws, "name": "s1", "caller": arch})
	code, out := doPost(t, h, "/v1/blueprints/verdict", map[string]any{
		"workspace_root": ws, "name": "s1", "verdict": "changes", "via": "cli", "notes": "fix the 500",
	})
	if code != 200 || out["status"] != "active" || out["follow_up_phase"] != float64(2) {
		t.Fatalf("changes: %d %v", code, out)
	}
	if !mailContaining(takeMail(t, h, ws, arch), "fix the 500") {
		t.Fatal("architect not given the human's notes")
	}
	if mail := takeMail(t, h, ws, build); len(mail) != 0 {
		t.Fatalf("builder mail before follow-up spec = %v, want none", mail)
	}
	spec(t, h, ws, "s1", arch, 2, "Review fixes (round 1)")
	if !mailContaining(takeMail(t, h, ws, build), "Phase 2") {
		t.Fatal("builder not given the follow-up spec")
	}
}
