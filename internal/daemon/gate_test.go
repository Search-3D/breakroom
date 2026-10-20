package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gateFixture registers an architect (wt-a) and builder (wt-b) with
// blueprint "bp" on an approved roadmap of these titles, mail drained.
func gateFixture(t *testing.T, titles ...string) (http.Handler, string, string) {
	t.Helper()
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	if code, out := doPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	}); code != 200 {
		t.Fatalf("init: %d %v", code, out)
	}
	approveRoadmap(t, h, "/ws", "bp", arch, titles...)
	return h, arch, build
}

// mustPost posts body to path, failing the test on a non-200 reply.
func mustPost(t *testing.T, h http.Handler, path string, body map[string]any) {
	t.Helper()
	if code, out := doPost(t, h, path, body); code != 200 {
		t.Fatalf("%s: %d %v", path, code, out)
	}
}

// specPhase1 has the architect spec phase 1 of blueprint "bp".
func specPhase1(t *testing.T, h http.Handler, arch string) {
	t.Helper()
	mustPost(t, h, "/v1/blueprints/spec", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"title": "t", "objective": "o",
		"requirements": []any{map[string]any{"id": "r1", "text": "req"}},
		"acceptance":   []any{map[string]any{"id": "a1", "text": "acc"}},
	})
}

// submitPhase1 has the builder deliver phase 1 of blueprint "bp".
func submitPhase1(t *testing.T, h http.Handler, build string) {
	t.Helper()
	mustPost(t, h, "/v1/blueprints/submit", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": build, "phase": 1,
		"items": []any{map[string]any{"requirement_id": "r1", "summary": "done"}},
		"proof": "ok",
	})
}

// signoffPhase1 has the architect sign off phase 1 of blueprint "bp".
func signoffPhase1(t *testing.T, h http.Handler, arch string) {
	t.Helper()
	mustPost(t, h, "/v1/blueprints/inspect", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1,
		"verdict": "signoff", "acceptance_ids": []any{"a1"},
	})
}

// gate runs the stop gate for the agent in wt and returns its decision.
func gate(t *testing.T, h http.Handler, wt string, active bool, hold int) map[string]any {
	t.Helper()
	code, out := doPost(t, h, "/v1/gate/stop", map[string]any{
		"workspace_root": "/ws", "worktree_path": wt, "stop_hook_active": active, "hold_sec": hold,
	})
	if code != 200 {
		t.Fatalf("gate: %d %v", code, out)
	}
	return out
}

// wantDecision fails the test unless the gate returned action (and kind, if set).
func wantDecision(t *testing.T, out map[string]any, action, kind string) {
	t.Helper()
	if out["action"] != action || (kind != "" && out["kind"] != kind) {
		t.Fatalf("decision = %v, want %s/%s", out, action, kind)
	}
}

func TestGateMailBlocks(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": build, "to": arch, "content": "cart API ready"})
	out := gate(t, h, "/ws/wt-a", false, 1)
	wantDecision(t, out, "block", "mail")
	if ids, _ := out["message_ids"].([]any); len(ids) != 1 || !strings.Contains(out["reason"].(string), "cart API ready") || out["agent"] != arch {
		t.Fatalf("decision = %v", out)
	}
}

func TestGateStopPeeksDoesNotTake(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": build, "to": arch, "content": "hi"})
	gate(t, h, "/ws/wt-a", false, 1)
	if mail := takeMail(t, h, "/ws", arch); len(mail) != 1 {
		t.Fatalf("gate consumed mail: %v", mail)
	}
}

// While the human weighs the roadmap the architect may stop; the builder
// parks through roadmap approval until phase 1 is specced.
func TestGateHumanPendingHoldsBuilder(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws/wt-a", "architect")
	build := register(t, h, "/ws", "/ws/wt-b", "builder")
	mustPost(t, h, "/v1/blueprints/init", map[string]any{"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build})
	mustPost(t, h, "/v1/blueprints/roadmap", map[string]any{"workspace_root": "/ws", "name": "bp", "caller": arch,
		"phases": []any{map[string]any{"phase": 1, "title": "t"}}})
	wantDecision(t, gate(t, h, "/ws/wt-a", false, 1), "allow", "")
	out := gate(t, h, "/ws/wt-b", false, 1)
	wantDecision(t, out, "block", "hold_expired")
	if !strings.Contains(out["reason"].(string), "roadmap or next spec") {
		t.Fatalf("reason = %v", out["reason"])
	}
	mustPost(t, h, "/v1/blueprints/approve", map[string]any{"workspace_root": "/ws", "name": "bp", "via": "cli"})
	wantDecision(t, gate(t, h, "/ws/wt-b", true, 1), "block", "hold_expired") // approved, phase 1 not specced
	specPhase1(t, h, arch)
	wantDecision(t, gate(t, h, "/ws/wt-b", true, 1), "block", "mail")
}

func TestGateBlockedAllows(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	specPhase1(t, h, arch)
	for range 6 { // default cap is 5 rounds
		submitPhase1(t, h, build)
		mustPost(t, h, "/v1/blueprints/inspect", map[string]any{
			"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 1, "verdict": "punchlist",
			"findings": []any{map[string]any{"requirement_id": "r1", "issue": "x", "fix": "y"}},
		})
	}
	takeMail(t, h, "/ws", arch)
	takeMail(t, h, "/ws", build)
	wantDecision(t, gate(t, h, "/ws/wt-a", false, 1), "allow", "")
	wantDecision(t, gate(t, h, "/ws/wt-b", false, 1), "allow", "")
}

func TestGateBuilderInspectionHolds(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	specPhase1(t, h, arch)
	submitPhase1(t, h, build)
	takeMail(t, h, "/ws", build)
	start := time.Now()
	out := gate(t, h, "/ws/wt-b", false, 1)
	wantDecision(t, out, "block", "hold_expired")
	if time.Since(start) < time.Second || !strings.Contains(out["reason"].(string), arch) {
		t.Fatalf("hold = %v after %v", out, time.Since(start))
	}
}

func TestGateBuilderInspectionHoldsSharedRoot(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws", "architect")
	build := register(t, h, "/ws", "/ws/.workspaces/bp", "builder")
	mustPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	approveRoadmap(t, h, "/ws", "bp", arch, "t")
	specPhase1(t, h, arch)
	submitPhase1(t, h, build)
	takeMail(t, h, "/ws", build)

	// Builder ends turn from the main checkout root (/ws) where Codex CLI runs.
	out := gate(t, h, "/ws", false, 1)
	wantDecision(t, out, "block", "hold_expired")
	if out["agent"] != build {
		t.Fatalf("gate resolved agent = %v, want builder %v", out["agent"], build)
	}
	if !strings.Contains(out["reason"].(string), arch) {
		t.Fatalf("reason = %v, want wait on architect %v", out["reason"], arch)
	}
}

func TestGateBuilderInspectionWakesOnInspectSharedRoot(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws", "architect")
	build := register(t, h, "/ws", "/ws/.workspaces/bp", "builder")
	mustPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	approveRoadmap(t, h, "/ws", "bp", arch, "t", "p2")
	specPhase1(t, h, arch)
	submitPhase1(t, h, build)
	takeMail(t, h, "/ws", build)

	// In the background, architect inspects phase 1 and specs phase 2.
	go func() {
		time.Sleep(200 * time.Millisecond)
		takeMail(t, h, "/ws", arch)
		signoffPhase1(t, h, arch)
		mustPost(t, h, "/v1/blueprints/spec", map[string]any{
			"workspace_root": "/ws", "name": "bp", "caller": arch, "phase": 2,
			"title": "p2", "objective": "p2 obj",
			"requirements": []any{map[string]any{"id": "r2", "text": "req2"}},
			"acceptance":   []any{map[string]any{"id": "a2", "text": "acc2"}},
		})
	}()

	// Builder was parked on Stop hook at /ws. It should wake up with the phase 2 spec mail!
	out := gate(t, h, "/ws", false, 5)
	wantDecision(t, out, "block", "mail")
	if out["agent"] != build {
		t.Fatalf("gate resolved agent = %v, want builder %v", out["agent"], build)
	}
	// Both agents share /ws with no host PIDs, so the agent is a guess: the
	// wake carries no content and no ack, so a wrong guess can't misdeliver.
	if out["reason"] != guessedWakeReason || out["suppress_ack"] != true {
		t.Fatalf("guessed wake = %v, want the generic reason without ack", out)
	}

	// Now Architect ends turn from /ws after speccing phase 2.
	// Architect should hold waiting on the builder!
	archOut := gate(t, h, "/ws", false, 1)
	wantDecision(t, archOut, "block", "hold_expired")
	if archOut["agent"] != arch {
		t.Fatalf("gate resolved agent = %v, want architect %v", archOut["agent"], arch)
	}
	if !strings.Contains(archOut["reason"].(string), build) {
		t.Fatalf("reason = %v, want wait on builder %v", archOut["reason"], build)
	}
}

func TestGateHoldWakesOnMail(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	specPhase1(t, h, arch)
	submitPhase1(t, h, build)
	takeMail(t, h, "/ws", build)
	go func() {
		time.Sleep(200 * time.Millisecond)
		// Raw request: t.Fatalf (mustPost, doPost) must not run off the test goroutine.
		body := fmt.Sprintf(`{"workspace_root":"/ws","from":%q,"to":%q,"content":"looks good"}`, arch, build)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Errorf("send: %d %s", rec.Code, rec.Body)
		}
	}()
	start := time.Now()
	out := gate(t, h, "/ws/wt-b", false, 5)
	wantDecision(t, out, "block", "mail")
	if time.Since(start) > 3*time.Second {
		t.Fatalf("woke after %v", time.Since(start))
	}
}

// After sign-off the builder parks until handover, so the next spec (or the
// close) wakes it instead of landing in an idle session.
func TestGateBuilderSignoffHolds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		titles []string
	}{
		{"two phases", []string{"t", "u"}},
		{"one phase", []string{"t"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, arch, build := gateFixture(t, tc.titles...)
			specPhase1(t, h, arch)
			submitPhase1(t, h, build)
			signoffPhase1(t, h, arch)
			takeMail(t, h, "/ws", build)
			out := gate(t, h, "/ws/wt-b", true, 1)
			wantDecision(t, out, "block", "hold_expired")
			if !strings.Contains(out["reason"].(string), "signed off; waiting on "+arch) {
				t.Fatalf("%s: reason = %v", tc.name, out["reason"])
			}
			if len(tc.titles) == 1 {
				mustPost(t, h, "/v1/blueprints/close", map[string]any{"workspace_root": "/ws", "name": "bp", "caller": arch})
				wantDecision(t, gate(t, h, "/ws/wt-b", true, 1), "block", "mail")
				takeMail(t, h, "/ws", build)
				wantDecision(t, gate(t, h, "/ws/wt-b", true, 1), "allow", "")
			}
		})
	}
}

func TestGateArchitectConstructingHolds(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	specPhase1(t, h, arch)
	takeMail(t, h, "/ws", build)
	wantDecision(t, gate(t, h, "/ws/wt-a", false, 1), "block", "hold_expired")
}

// A peer that went offline can't answer: remind once, then allow, instead
// of parking forever.
func TestGateOfflinePeerReleases(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	specPhase1(t, h, arch)
	takeMail(t, h, "/ws", build)
	mustPost(t, h, "/v1/agents/release", map[string]any{"workspace_root": "/ws", "agent_id": build})
	out := gate(t, h, "/ws/wt-a", false, 5)
	wantDecision(t, out, "block", "reminder")
	if !strings.Contains(out["reason"].(string), "offline") {
		t.Fatalf("reason = %v", out["reason"])
	}
	wantDecision(t, gate(t, h, "/ws/wt-a", true, 5), "allow", "")
}

// Agents trading mail can't keep each other from stopping forever.
func TestGateMailRunCapped(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	takeMail(t, h, "/ws", build)
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": arch, "to": build, "content": "ping"})
	for range maxMailRun {
		wantDecision(t, gate(t, h, "/ws/wt-b", true, 1), "block", "mail")
	}
	wantDecision(t, gate(t, h, "/ws/wt-b", true, 1), "allow", "")
	// A new turn (not hook-continued) delivers it again.
	wantDecision(t, gate(t, h, "/ws/wt-b", false, 1), "block", "mail")
}

func TestGateReminders(t *testing.T) {
	h, arch, build := gateFixture(t, "t", "u")
	specPhase1(t, h, arch)
	takeMail(t, h, "/ws", build)
	out := gate(t, h, "/ws/wt-b", false, 1)
	wantDecision(t, out, "block", "reminder")
	if want := "Breakroom: blueprint bp phase 1 is still open. Call blueprint_submit, or send_message the architect, before stopping."; out["reason"] != want {
		t.Fatalf("builder reminder = %q", out["reason"])
	}
	wantDecision(t, gate(t, h, "/ws/wt-b", true, 1), "allow", "")

	submitPhase1(t, h, build)
	takeMail(t, h, "/ws", arch)
	out = gate(t, h, "/ws/wt-a", false, 1)
	if want := "Breakroom: blueprint bp phase 1 awaits your inspection. Call blueprint_inspect before stopping."; out["reason"] != want {
		t.Fatalf("inspection reminder = %v", out)
	}
	signoffPhase1(t, h, arch)
	takeMail(t, h, "/ws", arch)
	out = gate(t, h, "/ws/wt-a", false, 1)
	wantDecision(t, out, "block", "reminder")
	if want := "Breakroom: blueprint bp phase 1 is signed off. Spec phase 2 or call blueprint_close."; out["reason"] != want {
		t.Fatalf("architect reminder = %q", out["reason"])
	}

	h2, arch2, build2 := gateFixture(t, "t")
	specPhase1(t, h2, arch2)
	submitPhase1(t, h2, build2)
	signoffPhase1(t, h2, arch2)
	takeMail(t, h2, "/ws", arch2)
	if out := gate(t, h2, "/ws/wt-a", false, 1); out["reason"] != "Breakroom: blueprint bp phase 1 is signed off. Call blueprint_close." {
		t.Fatalf("last-phase reminder = %v", out)
	}
}

func TestGateNoBlueprintAllows(t *testing.T) {
	h := newTestServer(t).Handler()
	register(t, h, "/ws", "/ws/wt-a", "solo")
	wantDecision(t, gate(t, h, "/ws/wt-a", false, 1), "allow", "")
}

func TestGateRecordsEmission(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": build, "to": arch, "content": "hi"})
	out := gate(t, h, "/ws/wt-a", false, 1)
	code, rec := doPost(t, h, "/v1/prompts/record", map[string]any{
		"workspace_root": "/ws", "worktree_path": "/ws/wt-a", "text": out["reason"],
	})
	if code != 200 || rec["recorded"] != false {
		t.Fatalf("echo recorded: %d %v", code, rec)
	}
}

// Hooks must only speak for the agent whose worktree contains them: with a
// single live agent, a hook from another worktree is nobody's.
func TestHooksNeedContainingWorktree(t *testing.T) {
	h := newTestServer(t).Handler()
	register(t, h, "/ws", "/ws/wt-a", "architect")
	for _, tc := range []struct {
		name string
		path string
		body map[string]any
	}{
		{
			name: "prompt record",
			path: "/v1/prompts/record",
			body: map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/wt-b", "text": "approved"},
		},
		{
			name: "gate stop",
			path: "/v1/gate/stop",
			body: map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/wt-b", "hold_sec": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doPost(t, h, tc.path, tc.body)
			wantErr(t, code, out, "no_agents")
		})
	}
	// take keeps its single-agent fallback for `breakroom check`.
	if code, out := doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/wt-b"}); code != 200 {
		t.Fatalf("take = %d %v", code, out)
	}
}

// When multiple agents share a worktree (e.g. host running a shared daemon or
// builder parked in main checkout), prompt recording must not fail with
// ambiguous_agent, and must record the prompt for the architect.
func TestPromptRecordSharedWorktree(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws", "architect")
	build := register(t, h, "/ws", "/ws", "builder")

	mustPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	mustPost(t, h, "/v1/blueprints/roadmap", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "out_of_scope": "none",
		"phases": []any{map[string]any{"phase": 1, "title": "t1", "delivers": "d1"}},
	})

	code, out := doPost(t, h, "/v1/prompts/record", map[string]any{
		"workspace_root": "/ws", "worktree_path": "/ws", "text": "approved",
	})
	if code != 200 || out["recorded"] != true {
		t.Fatalf("prompt record on shared worktree = %d %v, want 200 recorded", code, out)
	}
	if out["agent"] != "" {
		t.Fatalf("recorded agent = %v, want unnamed (a guess must not deliver mail)", out["agent"])
	}

	// Verify the architect can now approve the roadmap via chat using the recorded quote!
	code, out = doPost(t, h, "/v1/blueprints/approve", map[string]any{
		"workspace_root": "/ws", "name": "bp", "caller": arch, "via": "chat", "quote": "approved",
	})
	if code != 200 {
		t.Fatalf("roadmap approve via chat failed: %d %v", code, out)
	}
}

// With two architects in one folder and no host to tell them apart, a
// prompt backs neither: one team's "go" must not approve another's roadmap.
func TestPromptRecordTwoArchitectsRecordsNobody(t *testing.T) {
	h := newTestServer(t).Handler()
	register(t, h, "/ws", "/ws", "architect")
	register(t, h, "/ws", "/ws", "architect")
	code, out := doPost(t, h, "/v1/prompts/record", map[string]any{
		"workspace_root": "/ws", "worktree_path": "/ws", "text": "go",
	})
	wantErr(t, code, out, "ambiguous_agent")
}

// A hook killed mid-hold (host timeout) must not consume mail.
func TestGateCancelledHoldKeepsMail(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	specPhase1(t, h, arch)
	submitPhase1(t, h, build)
	takeMail(t, h, "/ws", build)
	ctx, cancel := context.WithCancel(context.Background())
	body := strings.NewReader(`{"workspace_root":"/ws","worktree_path":"/ws/wt-b","hold_sec":30}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/gate/stop", body).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": arch, "to": build, "content": "verdict"})
	if mail := takeMail(t, h, "/ws", build); len(mail) != 1 {
		t.Fatalf("mail after cancelled hold = %v", mail)
	}
}

// A roadmap revision mid-phase doesn't pause the build: the builder is
// still reminded to submit, not parked.
func TestGateRevisionMidPhaseKeepsReminder(t *testing.T) {
	h, arch, build := gateFixture(t, "t")
	specPhase1(t, h, arch)
	mustPost(t, h, "/v1/blueprints/roadmap", map[string]any{"workspace_root": "/ws", "name": "bp", "caller": arch,
		"phases": []any{map[string]any{"phase": 1, "title": "t"}, map[string]any{"phase": 2, "title": "u"}}})
	takeMail(t, h, "/ws", build)
	wantDecision(t, gate(t, h, "/ws/wt-b", false, 1), "block", "reminder")
}

// In a shared worktree where multiple agents run, concurrent stop hooks (such as
// duplicate hooks configured by host/project) must resolve to the same agent and
// not flip to the peer, which would deliver cross-agent reminders or allow premature stop.
func TestGateConcurrentStopHooksResolveToSameAgent(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws", "architect")
	build := register(t, h, "/ws", "/ws", "builder")

	mustPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	approveRoadmap(t, h, "/ws", "bp", arch, "t")
	specPhase1(t, h, arch)
	takeMail(t, h, "/ws", build)

	// Builder submits phase 1. Builder is now waiting for inspection.
	submitPhase1(t, h, build)

	// Hook 1: Builder stop hook starts and holds in s.park.
	hook1Done := make(chan map[string]any, 1)
	go func() {
		hook1Done <- gate(t, h, "/ws", false, 5)
	}()

	// Give Hook 1 a moment to enter park.
	time.Sleep(50 * time.Millisecond)

	// Hook 2: A duplicate/concurrent stop hook arrives for /ws while Hook 1 is holding.
	// It must NOT resolve to the architect (which would return "call blueprint_inspect").
	// It must resolve to the builder and also hold.
	hook2Done := make(chan map[string]any, 1)
	go func() {
		hook2Done <- gate(t, h, "/ws", false, 5)
	}()

	time.Sleep(50 * time.Millisecond)

	// Neither hook should have completed with allow or inspection reminder.
	select {
	case res := <-hook1Done:
		t.Fatalf("hook 1 returned early: %v", res)
	case res := <-hook2Done:
		t.Fatalf("hook 2 returned early (likely resolved to peer architect): %v", res)
	default:
	}

	// Now architect sends review feedback / message to builder.
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": arch, "to": build, "content": "review complete"})

	// Both hooks should now wake with mail for the builder.
	var res1, res2 map[string]any
	select {
	case res1 = <-hook1Done:
	case <-time.After(2 * time.Second):
		t.Fatal("hook 1 timed out waiting for mail")
	}
	select {
	case res2 = <-hook2Done:
	case <-time.After(2 * time.Second):
		t.Fatal("hook 2 timed out waiting for mail")
	}

	if res1["agent"] != build {
		t.Fatalf("hook 1 resolved to %v, want %s", res1["agent"], build)
	}
	if res2["agent"] != build {
		t.Fatalf("hook 2 resolved to %v, want %s", res2["agent"], build)
	}
	wantDecision(t, res1, "block", "mail")
	wantDecision(t, res2, "block", "mail")
}

// In a shared worktree, the most recent MCP caller is recognized as the stopping agent,
// even if neither agent is parked.
func TestGateSharedWorktreeRecentMCPCaller(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws", "architect")
	build := register(t, h, "/ws", "/ws", "builder")

	mustPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	approveRoadmap(t, h, "/ws", "bp", arch, "t")
	specPhase1(t, h, arch)
	takeMail(t, h, "/ws", build)

	// Builder submits phase 1.
	submitPhase1(t, h, build)

	// Builder stops (with a short hold).
	hookDone := make(chan map[string]any, 1)
	go func() {
		hookDone <- gate(t, h, "/ws", false, 1)
	}()

	time.Sleep(50 * time.Millisecond)
	// Architect signs off, unblocking builder.
	signoffPhase1(t, h, arch)

	select {
	case res := <-hookDone:
		if res["agent"] != build {
			t.Fatalf("gate resolved to %v, want builder %s", res["agent"], build)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate timed out")
	}
}

// When the architect specs a phase and concurrent stop hooks run, both hooks
// must resolve to the architect and hold (not allow or resolve to builder),
// so that when the builder submits, the architect wakes properly.
func TestGateConcurrentStopHooksArchitectSpeccing(t *testing.T) {
	h := newTestServer(t).Handler()
	arch := register(t, h, "/ws", "/ws", "architect")
	build := register(t, h, "/ws", "/ws", "builder")

	mustPost(t, h, "/v1/blueprints/init", map[string]any{
		"workspace_root": "/ws", "name": "bp", "architect": arch, "builder": build,
	})
	approveRoadmap(t, h, "/ws", "bp", arch, "t")

	// Architect specs phase 1. Architect should now hold waiting for builder construction.
	specPhase1(t, h, arch)

	// Hook 1 starts for architect with stop_hook_active=true (Codex mode).
	hook1Done := make(chan map[string]any, 1)
	go func() {
		hook1Done <- gate(t, h, "/ws", true, 5)
	}()

	time.Sleep(50 * time.Millisecond)

	// Hook 2 arrives concurrently while Hook 1 is holding.
	// In the old buggy code, Hook 2 resolved to builder, saw builder had no hold,
	// and returned action: allow, causing Codex to close the architect's turn!
	hook2Done := make(chan map[string]any, 1)
	go func() {
		hook2Done <- gate(t, h, "/ws", true, 5)
	}()

	time.Sleep(50 * time.Millisecond)

	// Neither hook should return allow early.
	select {
	case res := <-hook1Done:
		t.Fatalf("hook 1 returned early: %v", res)
	case res := <-hook2Done:
		t.Fatalf("hook 2 returned early (premature allow): %v", res)
	default:
	}

	// Builder drains the spec mail and submits phase 1.
	takeMail(t, h, "/ws", build)
	submitPhase1(t, h, build)

	// Both hooks must wake with the submission mail for the architect!
	var res1, res2 map[string]any
	select {
	case res1 = <-hook1Done:
	case <-time.After(2 * time.Second):
		t.Fatal("hook 1 timed out waiting for submission mail")
	}
	select {
	case res2 = <-hook2Done:
	case <-time.After(2 * time.Second):
		t.Fatal("hook 2 timed out waiting for submission mail")
	}

	if res1["agent"] != arch {
		t.Fatalf("hook 1 resolved to %v, want architect %s", res1["agent"], arch)
	}
	if res2["agent"] != arch {
		t.Fatalf("hook 2 resolved to %v, want architect %s", res2["agent"], arch)
	}
	wantDecision(t, res1, "block", "mail")
	wantDecision(t, res2, "block", "mail")
}

func TestGateInvalidHTTPMethod(t *testing.T) {
	h := newTestServer(t).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/gate/stop", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/gate/stop code = %d, want 405 Method Not Allowed", rec.Code)
	}
}

// A guessed agent's waiting mail neither blocks the stop nor gets acked:
// the stopping chat may be someone else, and the owner must still read it.
func TestGateGuessNeverDeliversWaitingMail(t *testing.T) {
	h := newTestServer(t).Handler()
	a := register(t, h, "/ws", "/ws", "solo")
	b := register(t, h, "/ws", "/ws", "solo")
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": a, "to": b, "content": "for b only"})
	if out := gate(t, h, "/ws", false, 1); out["action"] != "allow" {
		t.Fatalf("guessed gate = %v, want allow", out)
	}
	if mail := takeMail(t, h, "/ws", b); len(mail) != 1 {
		t.Fatalf("b's mail = %v, want it still unread", mail)
	}
}

// A guess between agents sharing a folder never picks an agent from another
// worktree, even one that called a tool more recently.
func TestGateGuessStaysInWorktree(t *testing.T) {
	h := newTestServer(t).Handler()
	a := register(t, h, "/ws", "/ws/wt-a", "architect")
	b := register(t, h, "/ws", "/ws/wt-a", "builder")
	c := register(t, h, "/ws", "/ws/wt-c", "builder")
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": b, "to": a, "content": "hi"})
	mustPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": c, "to": a, "content": "hi"})
	if out := gate(t, h, "/ws/wt-a", false, 0); out["agent"] != b {
		t.Fatalf("gate guessed %v, want %s (not %s from wt-c)", out["agent"], b, c)
	}
}
