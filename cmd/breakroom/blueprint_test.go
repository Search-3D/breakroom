package main

import (
	"os"
	"strings"
	"testing"
)

// wantFail fails the test unless env is an error envelope with the given code.
func wantFail(t *testing.T, env map[string]any, code string) {
	t.Helper()
	if env["ok"] != false {
		t.Fatalf("envelope ok, want failure: %v", env)
	}
	errStr, _ := env["error"].(string)
	if !strings.HasPrefix(errStr, code+":") {
		t.Fatalf("error = %q, want %s:*", errStr, code)
	}
}

func TestMCPBlueprint(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e MCP test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-bp-")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp("", "br-bpws-")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run LIFO: stop first while home (pidfile) still exists.
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { os.RemoveAll(ws) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })

	initParams := map[string]any{
		"protocolVersion": "2026-07-28", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}
	a := startMCP(t, home, bin, ws)
	a.call("initialize", initParams)
	a.notify("notifications/initialized", map[string]any{})
	data := wantOK(t, a.tool("register_agent", map[string]any{"role": "architect", "worktree_path": ws}))
	a1, _ := data["agent_id"].(string)
	if a1 == "" {
		t.Fatalf("a registered as %v", data)
	}
	b := startMCP(t, home, bin, ws)
	b.call("initialize", initParams)
	b.notify("notifications/initialized", map[string]any{})
	data = wantOK(t, b.tool("register_agent", map[string]any{"role": "builder", "worktree_path": ws}))
	a2, _ := data["agent_id"].(string)
	if a2 == "" {
		t.Fatalf("b registered as %v", data)
	}

	data = wantOK(t, a.tool("blueprint_init", map[string]any{
		"name": "bp", "builder": a2, "goal": "g",
	}))
	if data["status"] != "drafting" || data["builder"] != a2 {
		t.Fatalf("init = %v", data)
	}
	wantOK(t, a.tool("blueprint_roadmap", map[string]any{"name": "bp",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}, map[string]any{"phase": 2, "title": "t"}}}))
	if out, code := runIn(t, home, bin, ws, "blueprint", "approve", "bp"); code != 0 {
		t.Fatalf("approve = %d %q", code, out)
	}
	runIn(t, home, bin, ws, "check", "--agent", a2) // drop the roadmap mail
	runIn(t, home, bin, ws, "check", "--agent", a1)
	// Builder cannot spec.
	wantFail(t, b.tool("blueprint_spec", map[string]any{
		"name": "bp", "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}), "forbidden_role")

	spec := map[string]any{
		"name": "bp", "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}
	data = wantOK(t, a.tool("blueprint_spec", spec))
	if data["status"] != "constructing" {
		t.Fatalf("spec = %v", data)
	}
	// Spec mail reaches the builder via piggyback.
	env := b.tool("blueprint_status", map[string]any{"name": "bp"})
	unread, _ := env["_unread_messages"].([]any)
	if len(unread) != 1 || unread[0].(map[string]any)["type"] != "blueprint_spec" {
		t.Fatalf("unread = %v", unread)
	}

	submit := map[string]any{
		"name": "bp", "phase": 1,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "done"}},
		"proof": "ok",
	}
	data = wantOK(t, b.tool("blueprint_submit", submit))
	if data["status"] != "inspection" {
		t.Fatalf("submit = %v", data)
	}
	punch := map[string]any{
		"name": "bp", "phase": 1, "verdict": "punchlist",
		"findings": []any{map[string]any{
			"requirement_id": "r1", "issue": "gap", "fix": "fill",
		}},
	}
	// Builder cannot inspect (self-sign-off is impossible).
	selfSign := map[string]any{
		"name": "bp", "phase": 1, "verdict": "signoff",
		"acceptance_ids": []any{"a1"},
	}
	wantFail(t, b.tool("blueprint_inspect", selfSign), "forbidden_role")
	data = wantOK(t, a.tool("blueprint_inspect", punch))
	if data["status"] != "punchlist" || data["rounds"] != float64(1) {
		t.Fatalf("punch = %v", data)
	}
	// Some clients (Muse) send array arguments JSON-encoded as strings.
	submit["items"] = `[{"requirement_id": "r1", "summary": "done"}]`
	data = wantOK(t, b.tool("blueprint_submit", submit))
	if data["status"] != "inspection" {
		t.Fatalf("resubmit = %v", data)
	}
	data = wantOK(t, a.tool("blueprint_inspect", map[string]any{
		"name": "bp", "phase": 1, "verdict": "signoff",
		"acceptance_ids": []any{"a1"},
	}))
	if data["status"] != "signoff" {
		t.Fatalf("signoff = %v", data)
	}

	// Phase 2 straight through, then handover.
	spec["phase"] = 2
	data = wantOK(t, a.tool("blueprint_spec", spec))
	if data["status"] != "constructing" {
		t.Fatalf("spec2 = %v", data)
	}
	submit["phase"] = 2
	data = wantOK(t, b.tool("blueprint_submit", submit))
	if data["status"] != "inspection" {
		t.Fatalf("submit2 = %v", data)
	}
	data = wantOK(t, a.tool("blueprint_inspect", map[string]any{
		"name": "bp", "phase": 2, "verdict": "signoff",
		"acceptance_ids": []any{"a1"},
	}))
	if data["status"] != "signoff" {
		t.Fatalf("signoff2 = %v", data)
	}
	wantFail(t, b.tool("blueprint_close", map[string]any{"name": "bp"}), "forbidden_role")
	data = wantOK(t, a.tool("blueprint_close", map[string]any{"name": "bp"}))
	if data["status"] != "handover" {
		t.Fatalf("close = %v", data)
	}
	data = wantOK(t, a.tool("blueprint_status", map[string]any{"name": "bp"}))
	bp, _ := data["blueprint"].(map[string]any)
	phases, _ := bp["phases"].([]any)
	if bp["status"] != "handover" || len(phases) != 2 {
		t.Fatalf("status = %v", data)
	}

	// Second blueprint burns all 5 rounds and blocks.
	if data := wantOK(t, a.tool("blueprint_init", map[string]any{
		"name": "stuck", "builder": a2,
	})); data["status"] != "drafting" {
		t.Fatalf("stuck init = %v", data)
	}
	wantOK(t, a.tool("blueprint_roadmap", map[string]any{"name": "stuck",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}}}))
	if out, code := runIn(t, home, bin, ws, "blueprint", "approve", "stuck"); code != 0 {
		t.Fatalf("approve stuck = %d %q", code, out)
	}
	runIn(t, home, bin, ws, "check", "--agent", a2) // drop the roadmap mail
	runIn(t, home, bin, ws, "check", "--agent", a1)
	spec["name"] = "stuck"
	spec["phase"] = 1
	data = wantOK(t, a.tool("blueprint_spec", spec))
	submit["name"] = "stuck"
	submit["phase"] = 1
	punch["name"] = "stuck"
	for i := 1; i <= 6; i++ {
		data = wantOK(t, b.tool("blueprint_submit", submit))
		if data["status"] != "inspection" {
			t.Fatalf("stuck submit %d = %v", i, data)
		}
		data = wantOK(t, a.tool("blueprint_inspect", punch))
		if i < 6 && (data["status"] != "punchlist" || data["rounds"] != float64(i)) {
			t.Fatalf("stuck punch %d = %v", i, data)
		}
	}
	if data["status"] != "blocked" {
		t.Fatalf("stuck punch 6 = %v, want blocked", data)
	}
	wantFail(t, b.tool("blueprint_submit", submit), "blueprint_blocked")
}

// signedOffViaMCP runs a 1-phase blueprint "bp" through sign-off over MCP.
func signedOffViaMCP(t *testing.T, e *gateEnv) {
	t.Helper()
	wantOK(t, e.a.tool("blueprint_init", map[string]any{"name": "bp", "builder": e.build}))
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{"name": "bp",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}}}))
	if out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "approve", "bp"); code != 0 {
		t.Fatalf("approve = %d %q", code, out)
	}
	wantOK(t, e.a.tool("blueprint_spec", map[string]any{
		"name": "bp", "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}))
	wantOK(t, e.b.tool("blueprint_submit", map[string]any{"name": "bp", "phase": 1, "proof": "ok",
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "done"}}}))
	wantOK(t, e.a.tool("blueprint_inspect", map[string]any{"name": "bp", "phase": 1,
		"verdict": "signoff", "acceptance_ids": []any{"a1"}}))
}

func TestBlueprintAcceptCLI(t *testing.T) {
	e := twoAgents(t)
	signedOffViaMCP(t, e)
	data := wantOK(t, e.a.tool("blueprint_request_acceptance", map[string]any{"name": "bp"}))
	if h, _ := data["handoff"].(string); !strings.Contains(h, "Reply accept") {
		t.Fatalf("request = %v", data)
	}
	if out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "reject", "bp"); code != 2 {
		t.Fatalf("reject without --notes = %d %q, want 2", code, out)
	}
	out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "accept", "bp")
	if code != 0 || !strings.Contains(out, "pull request") {
		t.Fatalf("accept = %d %q", code, out)
	}
	if bp := wantOK(t, e.a.tool("blueprint_status", map[string]any{"name": "bp"}))["blueprint"].(map[string]any); bp["status"] != "accepted" {
		t.Fatalf("status = %v", bp["status"])
	}
}

func TestBlueprintLandCLI(t *testing.T) {
	e := twoAgents(t)
	signedOffViaMCP(t, e)
	wantOK(t, e.a.tool("blueprint_request_acceptance", map[string]any{"name": "bp"}))
	if out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "accept", "bp"); code != 0 {
		t.Fatalf("accept = %d %q", code, out)
	}
	if out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "land", "bp"); code != 2 {
		t.Fatalf("land without --method = %d %q, want 2", code, out)
	}
	if out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "land", "bp", "--method", "invalid"); code != 2 {
		t.Fatalf("land with invalid --method = %d %q, want 2", code, out)
	}
	wantFail(t, e.a.tool("blueprint_land", map[string]any{"name": "bp", "method": "invalid", "quote": "ok"}), "bad_request")
	out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "land", "bp", "--method", "pr", "--ref", "https://example.test/pr/1")
	if code != 0 || !strings.Contains(out, "https://example.test/pr/1") {
		t.Fatalf("land = %d %q", code, out)
	}
}

func TestMCPSeriesCloseHandoffAndStatus(t *testing.T) {
	e := twoAgents(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	sh(t, e.ws, "init", "-b", "main", ".")
	writeGitFile(t, e.ws, "README.md", "init\n")
	sh(t, e.ws, "add", "-A")
	sh(t, e.ws, "commit", "-m", "init")

	data := wantOK(t, e.a.tool("blueprint_init", map[string]any{"name": "ser1"}))
	if data["builder"] != "" {
		t.Fatalf("want empty builder, got %v", data["builder"])
	}
	joinData := wantOK(t, e.b.tool("blueprint_join", map[string]any{"name": "ser1"}))
	if joinData["joined"] != true {
		t.Fatalf("join = %v", joinData)
	}
	wantOK(t, e.a.tool("blueprint_roadmap", map[string]any{
		"name": "ser1", "base": "main",
		"phases": []any{map[string]any{"phase": 1, "title": "t"}},
	}))
	out, code := runIn(t, e.home, e.bin, e.ws, "blueprint", "approve", "ser1")
	if code != 0 {
		t.Fatalf("approve = %d %q", code, out)
	}

	st := wantOK(t, e.a.tool("blueprint_status", map[string]any{"name": "ser1"}))
	bp, _ := st["blueprint"].(map[string]any)
	if bp["series"] != true || bp["worktree_path"] == "" || bp["branch"] == "" || bp["base_ref"] == "" {
		t.Fatalf("status dropped fields: %v", bp)
	}
	roadmaps, _ := bp["roadmaps"].([]any)
	if len(roadmaps) == 0 || roadmaps[0].(map[string]any)["base"] != "main" {
		t.Fatalf("roadmap base dropped: %v", bp)
	}

	closeData := wantOK(t, e.a.tool("blueprint_close", map[string]any{"name": "ser1"}))
	if closeData["status"] != "handover" {
		t.Fatalf("status = %v", closeData["status"])
	}
	handoff, _ := closeData["handoff"].(string)
	if handoff == "" || !strings.Contains(handoff, "without landing") {
		t.Fatalf("handoff = %q", handoff)
	}
}
