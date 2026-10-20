package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/search-3d/breakroom/internal/render"
)

// takeMail returns the recipient's unread mail and marks it read.
func takeMail(t *testing.T, h http.Handler, ws, recipient string) []map[string]any {
	t.Helper()
	code, out := doPost(t, h, "/v1/messages/take", map[string]any{
		"workspace_root": ws, "recipient": recipient,
	})
	if code != 200 {
		t.Fatalf("take: code=%d out=%v", code, out)
	}
	msgs, _ := out["messages"].([]any)
	out2 := []map[string]any{}
	for _, m := range msgs {
		out2 = append(out2, m.(map[string]any))
	}
	return out2
}

func TestBlueprintEndpoints(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")

	code, out := doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp",
		"architect": arch, "builder": build, "goal": "g",
	})
	if code != 200 || out["status"] != "drafting" {
		t.Fatalf("init = %d %v", code, out)
	}
	approveRoadmap(t, h, "/ws", "bp", arch, "t", "t")
	code, out = doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp2",
		"architect": arch, "builder": "agent-9",
	})
	wantErr(t, code, out, "unknown_agent")
	code, out = doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp",
		"architect": arch, "builder": build,
	})
	wantErr(t, code, out, "bad_request")

	spec := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}
	wrongRole := map[string]any{}
	for k, v := range spec {
		wrongRole[k] = v
	}
	wrongRole["caller"] = build
	code, out = doPost(t, h, "/v1/blueprints/spec", wrongRole)
	wantErr(t, code, out, "forbidden_role")
	phase2 := map[string]any{}
	for k, v := range spec {
		phase2[k] = v
	}
	phase2["phase"] = 2
	code, out = doPost(t, h, "/v1/blueprints/spec", phase2)
	wantErr(t, code, out, "bad_transition")
	code, out = doPost(t, h, "/v1/blueprints/spec", spec)
	if code != 200 || out["status"] != "constructing" {
		t.Fatalf("spec = %d %v", code, out)
	}
	mail := takeMail(t, h, "/ws", build)
	if len(mail) != 1 || mail[0]["type"] != "blueprint_spec" {
		t.Fatalf("builder mail = %v", mail)
	}
	if !strings.Contains(mail[0]["content"].(string), "[BLUEPRINT] bp") {
		t.Fatalf("spec content = %q", mail[0]["content"])
	}

	submit := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": build, "phase": 1,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "done"}},
		"proof": "ok",
	}
	submitArch := map[string]any{}
	for k, v := range submit {
		submitArch[k] = v
	}
	submitArch["caller"] = arch
	code, out = doPost(t, h, "/v1/blueprints/submit", submitArch)
	wantErr(t, code, out, "forbidden_role")
	code, out = doPost(t, h, "/v1/blueprints/submit", submit)
	if code != 200 || out["status"] != "inspection" {
		t.Fatalf("submit = %d %v", code, out)
	}
	mail = takeMail(t, h, "/ws", arch)
	if len(mail) != 1 || mail[0]["type"] != "review_req" {
		t.Fatalf("architect mail = %v", mail)
	}

	inspect := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"verdict": "punchlist",
		"findings": []any{map[string]any{
			"requirement_id": "r1", "issue": "gap", "fix": "fill",
		}},
	}
	inspectBuild := map[string]any{}
	for k, v := range inspect {
		inspectBuild[k] = v
	}
	inspectBuild["caller"] = build
	code, out = doPost(t, h, "/v1/blueprints/inspect", inspectBuild)
	wantErr(t, code, out, "forbidden_role")
	code, out = doPost(t, h, "/v1/blueprints/inspect", inspect)
	if code != 200 || out["status"] != "punchlist" || out["rounds"] != float64(1) {
		t.Fatalf("punch = %d %v", code, out)
	}
	mail = takeMail(t, h, "/ws", build)
	if len(mail) != 1 || mail[0]["type"] != "blueprint_punchlist" {
		t.Fatalf("punch mail = %v", mail)
	}
	code, out = doPost(t, h, "/v1/blueprints/submit", submit)
	if code != 200 {
		t.Fatalf("submit2 = %d %v", code, out)
	}
	_ = takeMail(t, h, "/ws", arch)
	signoff := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"verdict": "signoff", "acceptance_ids": []any{},
	}
	code, out = doPost(t, h, "/v1/blueprints/inspect", signoff)
	wantErr(t, code, out, "bad_request")
	signoff["acceptance_ids"] = []any{"a1"}
	code, out = doPost(t, h, "/v1/blueprints/inspect", signoff)
	if code != 200 || out["status"] != "signoff" {
		t.Fatalf("signoff = %d %v", code, out)
	}
	if mail = takeMail(t, h, "/ws", build); len(mail) != 0 {
		t.Fatalf("signoff mail = %v, want none until next spec or close", mail)
	}

	spec["phase"] = 2
	code, out = doPost(t, h, "/v1/blueprints/spec", spec)
	if code != 200 {
		t.Fatalf("spec2 = %d %v", code, out)
	}
	mail = takeMail(t, h, "/ws", build)
	if len(mail) != 1 || !strings.Contains(mail[0]["content"].(string), "Phase 1 signed off.") {
		t.Fatalf("spec2 mail = %v, want Phase 1 signed off note", mail)
	}
	code, out = doPost(t, h, "/v1/blueprints/close", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch,
	})
	wantErr(t, code, out, "bad_transition")
	submit["phase"] = 2
	code, out = doPost(t, h, "/v1/blueprints/submit", submit)
	if code != 200 {
		t.Fatalf("submit p2 = %d %v", code, out)
	}
	_ = takeMail(t, h, "/ws", arch)
	signoff["phase"] = 2
	code, out = doPost(t, h, "/v1/blueprints/inspect", signoff)
	if code != 200 {
		t.Fatalf("signoff p2 = %d %v", code, out)
	}
	_ = takeMail(t, h, "/ws", build)
	code, out = doPost(t, h, "/v1/blueprints/close", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch,
	})
	if code != 200 || out["status"] != "handover" {
		t.Fatalf("close = %d %v", code, out)
	}
	mail = takeMail(t, h, "/ws", build)
	if len(mail) != 1 || mail[0]["type"] != "chat" {
		t.Fatalf("handover mail = %v", mail)
	}
	code, out = doPost(t, h, "/v1/blueprints/close", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch,
	})
	if code != 200 {
		t.Fatalf("reclose = %d %v", code, out)
	}

	code, out = doPost(t, h, "/v1/blueprints/status", map[string]any{
		"workspace_root": "/ws", "name": "bp",
	})
	bp, _ := out["blueprint"].(map[string]any)
	phases, _ := bp["phases"].([]any)
	if code != 200 || bp["status"] != "handover" || len(phases) != 2 {
		t.Fatalf("status = %d %v", code, out)
	}
	code, out = doPost(t, h, "/v1/blueprints/status", map[string]any{
		"workspace_root": "/ws",
	})
	list, _ := out["blueprints"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	code, out = doPost(t, h, "/v1/blueprints/status", map[string]any{
		"workspace_root": "/ws", "name": "nope",
	})
	wantErr(t, code, out, "unknown_blueprint")
}

func TestBlueprintRoundCapEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.json"),
		[]byte(`{"blueprint_max_rounds": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t)
	h := srv.Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	if code, out := doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp",
		"architect": arch, "builder": build,
	}); code != 200 {
		t.Fatalf("init = %d %v", code, out)
	}
	approveRoadmap(t, h, "/ws", "bp", arch, "t", "t")
	spec := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}
	submit := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": build, "phase": 1,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "done"}},
		"proof": "ok",
	}
	inspect := map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"verdict": "punchlist",
		"findings": []any{map[string]any{
			"requirement_id": "r1", "issue": "gap", "fix": "fill",
		}},
	}
	if code, out := doPost(t, h, "/v1/blueprints/spec", spec); code != 200 {
		t.Fatalf("spec = %d %v", code, out)
	}
	if code, out := doPost(t, h, "/v1/blueprints/submit", submit); code != 200 {
		t.Fatalf("submit1 = %d %v", code, out)
	}
	code, out := doPost(t, h, "/v1/blueprints/inspect", inspect)
	if code != 200 || out["status"] != "punchlist" {
		t.Fatalf("punch1 = %d %v", code, out)
	}
	if code, out := doPost(t, h, "/v1/blueprints/submit", submit); code != 200 {
		t.Fatalf("submit2 = %d %v", code, out)
	}
	code, out = doPost(t, h, "/v1/blueprints/inspect", inspect)
	if code != 200 || out["status"] != "blocked" {
		t.Fatalf("punch2 = %d %v, want blocked", code, out)
	}
	code, out = doPost(t, h, "/v1/blueprints/submit", submit)
	wantErr(t, code, out, "blueprint_blocked")
}

// Real test output is big: the architect's mail stays small (items, notes,
// and the proof's head + tail), while blueprint_status phase=N serves the
// full proof and the overview stays slim.
func TestBlueprintBigProofMail(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	approveRoadmap(t, h, "/ws", "bp", arch, "t", "t")
	if code, out := doPost(t, h, "/v1/blueprints/spec", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}); code != 200 {
		t.Fatalf("spec = %d %v", code, out)
	}
	proof := "=== RUN first\n" + strings.Repeat("ok  pkg 0.1s ✓\n", 6000) + "FAIL: TestLast\n"
	if code, out := doPost(t, h, "/v1/blueprints/submit", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": build, "phase": 1,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "built the thing"}},
		"proof": proof, "notes": "go test: 6000 passed, 1 failed",
	}); code != 200 {
		t.Fatalf("submit = %d %v", code, out)
	}
	mail := takeMail(t, h, "/ws", arch)
	if len(mail) != 1 || mail[0]["type"] != "review_req" {
		t.Fatalf("architect mail = %d messages, want 1 review_req", len(mail))
	}
	content := mail[0]["content"].(string)
	if len(content) > 8<<10 || !utf8.ValidString(content) {
		t.Fatalf("mail = %d bytes (valid utf8: %v), want <= 8KB", len(content), utf8.ValidString(content))
	}
	for _, want := range []string{"built the thing", "6000 passed, 1 failed", "=== RUN first", "FAIL: TestLast", "phase=1"} {
		t.Run(want, func(t *testing.T) {
			if !strings.Contains(content, want) {
				t.Fatalf("mail lacks %q:\n%s", want, content)
			}
		})
	}
	// Overview: phases listed without heavy detail.
	_, out := doPost(t, h, "/v1/blueprints/status", map[string]any{"workspace_root": "/ws", "name": "bp"})
	ph := out["blueprint"].(map[string]any)["phases"].([]any)[0].(map[string]any)
	if ph["status"] != "inspection" || ph["deliverable"] != nil || ph["requirements"] != nil {
		t.Fatalf("overview phase = %v, want slim", ph)
	}
	// Phase detail: the full proof.
	code, out := doPost(t, h, "/v1/blueprints/status", map[string]any{"workspace_root": "/ws", "name": "bp", "phase": 1})
	phases, _ := out["blueprint"].(map[string]any)["phases"].([]any)
	if code != 200 || len(phases) != 1 {
		t.Fatalf("phase detail = %d %v", code, out)
	}
	if got := phases[0].(map[string]any)["deliverable"].(map[string]any)["proof"]; got != proof {
		t.Fatal("phase detail lost the full proof")
	}
	code, out = doPost(t, h, "/v1/blueprints/status", map[string]any{"workspace_root": "/ws", "name": "bp", "phase": 7})
	wantErr(t, code, out, "bad_request")
}

// The audit's hijack: builder goes offline, spec is mailed, a stranger
// registers. The stranger must get a fresh name, no builder powers, and the
// spec must wait for the real builder.
func TestBlueprintRoleSurvivesNewcomer(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	approveRoadmap(t, h, "/ws", "bp", arch, "t", "t")
	doPost(t, h, "/v1/agents/release", map[string]any{"workspace_root": "/ws", "agent_id": build})
	if code, out := doPost(t, h, "/v1/blueprints/spec", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	}); code != 200 {
		t.Fatalf("spec = %d %v", code, out)
	}
	stranger := register(t, h, "/ws", "/ws/wt-c", "docs-writer")
	if stranger == build {
		t.Fatalf("stranger inherited builder id %s", build)
	}
	if mail := takeMail(t, h, "/ws", stranger); len(mail) != 0 {
		t.Fatalf("stranger got %d messages, want 0", len(mail))
	}
	code, out := doPost(t, h, "/v1/blueprints/submit", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": stranger, "phase": 1,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "s"}}, "proof": "trust me",
	})
	wantErr(t, code, out, "forbidden_role")
	// The real builder reconnects (same worktree + role) and finds the spec.
	if back := register(t, h, "/ws", "/ws/wt-b", "builder"); back != build {
		t.Fatalf("builder reconnected as %s, want %s", back, build)
	}
	if mail := takeMail(t, h, "/ws", build); len(mail) != 1 || mail[0]["type"] != "blueprint_spec" {
		t.Fatalf("builder mail = %v, want the spec", mail)
	}
}

func TestTrimMiddle(t *testing.T) {
	marker := func(n int) string { return fmt.Sprintf("[%d omitted]\n", n) }
	for _, tc := range []struct {
		name   string
		in     string
		budget int
	}{
		{"fits", "short\n", 100},
		{"lines", strings.Repeat("line of text\n", 500), 1000},
		{"no newlines", strings.Repeat("x", 5000), 1000},
		{"multibyte", strings.Repeat("✓✓✓✓✓✓✓\n", 500), 1000},
		{"tiny budget", strings.Repeat("y\n", 500), 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := render.TrimMiddle(tc.in, tc.budget, marker)
			if len(tc.in) <= tc.budget {
				if got != tc.in {
					t.Errorf("changed input that fit")
				}
				return
			}
			if !utf8.ValidString(got) || len(got) > max(tc.budget, 160) || !strings.Contains(got, "omitted]") {
				t.Errorf("len=%d valid=%v got=%q", len(got), utf8.ValidString(got), got)
			}
			if tc.budget > 160 && !strings.HasSuffix(tc.in, got[strings.Index(got, "omitted]\n")+9:]) {
				t.Errorf("tail is not the input's suffix")
			}
		})
	}
}

// Handover mail says how the blueprint actually ended, and a repeat close
// (idempotent) does not notify again.
func TestBlueprintCloseMail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"blueprint_max_rounds": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	closeBP := func(name string) {
		t.Helper()
		if code, out := doPost(t, h, "/v1/blueprints/close", map[string]any{
			"workspace_root": "/ws", "name": name, "caller": arch,
		}); code != 200 {
			t.Fatalf("close %s = %d %v", name, code, out)
		}
	}
	// Drafting: nothing was ever specced.
	doPost(t, h, "/v1/blueprints/init", map[string]any{"workspace_root": "/ws", "name": "idea", "architect": arch, "builder": build})
	closeBP("idea")
	mail := takeMail(t, h, "/ws", build)
	if len(mail) != 1 || strings.Contains(mail[0]["content"].(string), "verified") ||
		!strings.Contains(mail[0]["content"].(string), "before any phase") {
		t.Fatalf("drafting close mail = %v", mail)
	}
	closeBP("idea")
	if mail := takeMail(t, h, "/ws", build); len(mail) != 0 {
		t.Fatalf("repeat close sent %d messages, want 0", len(mail))
	}
	// Blocked: round cap exhausted.
	doPost(t, h, "/v1/blueprints/init", map[string]any{"workspace_root": "/ws", "name": "stuck", "architect": arch, "builder": build})
	approveRoadmap(t, h, "/ws", "stuck", arch, "t")
	doPost(t, h, "/v1/blueprints/spec", map[string]any{
		"workspace_root": "/ws", "name": "stuck", "caller": arch, "phase": 1, "title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	})
	submit := map[string]any{"workspace_root": "/ws", "name": "stuck", "caller": build, "phase": 1,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "s"}}, "proof": "p"}
	punch := map[string]any{"workspace_root": "/ws", "name": "stuck", "caller": arch, "phase": 1, "verdict": "punchlist",
		"findings": []any{map[string]any{"requirement_id": "r1", "issue": "i", "fix": "f"}}}
	for range 2 {
		doPost(t, h, "/v1/blueprints/submit", submit)
		doPost(t, h, "/v1/blueprints/inspect", punch)
	}
	takeMail(t, h, "/ws", build)
	closeBP("stuck")
	mail = takeMail(t, h, "/ws", build)
	if len(mail) != 1 || strings.Contains(mail[0]["content"].(string), "verified") ||
		!strings.Contains(mail[0]["content"].(string), "BLOCKED") {
		t.Fatalf("blocked close mail = %v", mail)
	}
}

// capMail is the last line of defense on blueprint mail: never over the
// cap, never split a rune, short mail untouched.
func TestCapMail(t *testing.T) {
	if got := capMail("short"); got != "short" {
		t.Fatalf("short mail changed: %q", got)
	}
	for _, tc := range []struct{ name, s string }{
		{"ascii", strings.Repeat("a", mailMax+1)},
		{"multibyte", strings.Repeat("é", mailMax)},
		{"emoji", strings.Repeat("😀", mailMax)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capMail(tc.s)
			if len(got) > mailMax || !utf8.ValidString(got) || !strings.HasSuffix(got, "full text via blueprint_status]") {
				t.Fatalf("capMail(%d bytes) = %d bytes, valid=%v", len(tc.s), len(got), utf8.ValidString(got))
			}
		})
	}
}
