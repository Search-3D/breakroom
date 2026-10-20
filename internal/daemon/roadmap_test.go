package daemon

import (
	"net/http"
	"testing"
)

// approveRoadmap proposes a roadmap with these titles as the architect and
// approves it over the CLI route, for tests about later blueprint steps.
func approveRoadmap(t *testing.T, h http.Handler, ws, name, arch string, titles ...string) {
	t.Helper()
	phases := []any{}
	for i, title := range titles {
		phases = append(phases, map[string]any{"phase": i + 1, "title": title})
	}
	code, out := doPost(t, h, "/v1/blueprints/roadmap", map[string]any{
		"workspace_root": ws, "name": name, "caller": arch, "phases": phases,
	})
	if code != 200 {
		t.Fatalf("roadmap: %d %v", code, out)
	}
	code, out = doPost(t, h, "/v1/blueprints/approve", map[string]any{
		"workspace_root": ws, "name": name, "via": "cli",
	})
	if code != 200 {
		t.Fatalf("approve: %d %v", code, out)
	}
	_, out = doPost(t, h, "/v1/blueprints/status", map[string]any{"workspace_root": ws, "name": name})
	bp := out["blueprint"].(map[string]any)
	takeMail(t, h, ws, bp["builder"].(string)) // drop roadmap mail
	takeMail(t, h, ws, arch)
}

func TestRoadmapRoutes(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	if code, out := doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	}); code != 200 {
		t.Fatalf("init: %d %v", code, out)
	}
	roadmap := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": build,
		"phases": []any{map[string]any{"phase": 1, "title": "t", "delivers": "cart API"}},
	}
	code, out := doPost(t, h, "/v1/blueprints/roadmap", roadmap)
	wantErr(t, code, out, "forbidden_role")
	roadmap["caller"] = arch
	code, out = doPost(t, h, "/v1/blueprints/roadmap", roadmap)
	if code != 200 || out["version"] != float64(1) || out["status"] != "pending" {
		t.Fatalf("roadmap = %d %v", code, out)
	}
	if mail := takeMail(t, h, "/ws", build); len(mail) != 0 {
		t.Fatalf("builder mail on unapproved roadmap = %v, want none", mail)
	}

	spec := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}
	code, out = doPost(t, h, "/v1/blueprints/spec", spec)
	wantErr(t, code, out, "roadmap_unapproved")

	code, out = doPost(t, h, "/v1/prompts/record", map[string]any{
		"workspace_root": "/ws", "worktree_path": "/ws/wt-a", "text": "looks good, approved",
	})
	if code != 200 || out["agent"] != arch || out["recorded"] != true {
		t.Fatalf("record = %d %v", code, out)
	}
	approve := map[string]any{"workspace_root": "/ws", "name": "bp", "version": 1, "via": "chat", "caller": build, "quote": "approved"}
	code, out = doPost(t, h, "/v1/blueprints/approve", approve)
	wantErr(t, code, out, "forbidden_role")
	approve["caller"], approve["quote"] = arch, "nope"
	code, out = doPost(t, h, "/v1/blueprints/approve", approve)
	wantErr(t, code, out, "human_approval_required")
	approve["quote"] = "approved"
	code, out = doPost(t, h, "/v1/blueprints/approve", approve)
	if code != 200 || out["approved_via"] != "chat" || out["version"] != float64(1) {
		t.Fatalf("approve = %d %v", code, out)
	}
	if mail := takeMail(t, h, "/ws", build); len(mail) != 0 {
		t.Fatalf("approval mail to builder = %v, want none until spec", mail)
	}
	if code, out = doPost(t, h, "/v1/blueprints/spec", spec); code != 200 {
		t.Fatalf("spec after approval = %d %v", code, out)
	}

	code, out = doPost(t, h, "/v1/blueprints/status", map[string]any{"workspace_root": "/ws", "name": "bp"})
	if code != 200 {
		t.Fatalf("status = %d %v", code, out)
	}
	rms, _ := out["blueprint"].(map[string]any)["roadmaps"].([]any)
	if len(rms) != 1 {
		t.Fatalf("roadmaps = %v", out)
	}
	rm := rms[0].(map[string]any)
	if rm["approved_via"] != "chat" || rm["approval_quote"] != "approved" || rm["approved_at"] == nil {
		t.Fatalf("roadmap json = %v", rm)
	}
}

func TestApproveCLIRoute(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	doPost(t, h, "/v1/blueprints/roadmap", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch,
		"phases": []any{map[string]any{"phase": 1, "title": "t"}},
	})
	code, out := doPost(t, h, "/v1/blueprints/approve", map[string]any{"workspace_root": "/ws", "name": "bp", "via": "bogus"})
	wantErr(t, code, out, "bad_request")
	code, out = doPost(t, h, "/v1/blueprints/approve", map[string]any{"workspace_root": "/ws", "name": "bp", "via": "cli"})
	if code != 200 || out["approved_via"] != "cli" {
		t.Fatalf("cli approve = %d %v", code, out)
	}
	// The architect didn't make this call, so it is told.
	mail := takeMail(t, h, "/ws", arch)
	if len(mail) != 1 || mail[0]["type"] != "blueprint_roadmap" || mail[0]["from"] != "human" {
		t.Fatalf("architect mail = %v", mail)
	}
}

func TestAckMarksRead(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	code, out := doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": build, "to": arch, "content": "hi",
	})
	if code != 200 {
		t.Fatalf("send = %d %v", code, out)
	}
	ids := out["message_ids"].([]any)
	if code, out = doPost(t, h, "/v1/messages/ack", map[string]any{
		"workspace_root": "/ws", "recipient": arch, "ids": ids,
	}); code != 200 {
		t.Fatalf("ack = %d %v", code, out)
	}
	if mail := takeMail(t, h, "/ws", arch); len(mail) != 0 {
		t.Fatalf("mail after ack = %v", mail)
	}
}
