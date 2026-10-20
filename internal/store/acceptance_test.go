package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// signedOff returns a store with blueprint name whose 1-phase roadmap is
// approved and phase 1 signed off.
func signedOff(t *testing.T, name string) (*Store, string) {
	t.Helper()
	st := openBlueprintStore(t)
	ws, ctx := "/ws", context.Background()
	if err := st.InitBlueprint(ctx, ws, name, "arch", "build", "g"); err != nil {
		t.Fatal(err)
	}
	approveForTest(t, st, ws, name, "phase")
	if err := st.SpecPhase(ctx, testSpec(ws, name, 1)); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, name, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InspectPhase(ctx, InspectParams{WorkspaceRoot: ws, Name: name, Phase: 1, Verdict: "signoff", AcceptanceIDs: []string{"a1"}}); err != nil {
		t.Fatal(err)
	}
	return st, ws
}

func TestRequestAcceptance(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	if err := st.InitBlueprint(ctx, "/ws", "bp", "arch", "build", "g"); err != nil {
		t.Fatal(err)
	}
	approveForTest(t, st, "/ws", "bp", "phase")
	if err := st.SpecPhase(ctx, testSpec("/ws", "bp", 1)); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitPhase(ctx, testSubmit("/ws", "bp", 1)); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestAcceptance(ctx, "/ws", "bp"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("phase in inspection: err = %v, want ErrBadTransition", err)
	}

	st, ws := signedOff(t, "bp")
	if err := st.RequestAcceptance(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	bp, _ := st.GetBlueprint(ctx, ws, "bp")
	if bp.Status != BlueprintAcceptance || bp.AcceptRequestedAt == nil {
		t.Fatalf("bp = %+v, want acceptance", bp)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 2)); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("spec during acceptance: err = %v", err)
	}
}

func TestVerdictChatNeedsHuman(t *testing.T) {
	ctx := context.Background()
	st, ws := signedOff(t, "bp")
	if _, err := st.RecordPrompt(ctx, ws, "arch", "looks great, accept"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := st.RequestAcceptance(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	accept := VerdictParams{WorkspaceRoot: ws, Name: "bp", Verdict: "accept", Via: "chat", Caller: "arch", Quote: "accept"}
	if _, err := st.RecordVerdict(ctx, accept); !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("prompt from before the request: err = %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := st.RecordPrompt(ctx, ws, "arch", "yes, accept it"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordVerdict(ctx, accept); err != nil {
		t.Fatal(err)
	}
	bp, _ := st.GetBlueprint(ctx, ws, "bp")
	if bp.Status != BlueprintAccepted || bp.AcceptedAt == nil {
		t.Fatalf("bp = %+v, want accepted", bp)
	}
}

func TestVerdictChanges(t *testing.T) {
	ctx := context.Background()
	st, ws := signedOff(t, "bp")
	if err := st.RequestAcceptance(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordVerdict(ctx, VerdictParams{WorkspaceRoot: ws, Name: "bp", Verdict: "changes", Via: "cli"}); !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("changes without notes: err = %v", err)
	}
	n, err := st.RecordVerdict(ctx, VerdictParams{WorkspaceRoot: ws, Name: "bp", Verdict: "changes", Via: "cli", Notes: "fix the 500 on /users"})
	if err != nil || n != 2 {
		t.Fatalf("follow-up = %d (%v), want 2", n, err)
	}
	rms, _ := st.ListRoadmaps(ctx, ws, "bp")
	latest := rms[len(rms)-1]
	if latest.ApprovedAt == nil || len(latest.Phases) != 2 || latest.Phases[1].Title != "Review fixes (round 1)" || latest.Phases[1].Delivers != "fix the 500 on /users" {
		t.Fatalf("latest roadmap = %+v", latest)
	}
	bp, _ := st.GetBlueprint(ctx, ws, "bp")
	if bp.Status != BlueprintActive {
		t.Fatalf("status = %s, want active", bp.Status)
	}
	p2 := testSpec(ws, "bp", 2)
	p2.Title = "Review fixes (round 1)"
	if err := st.SpecPhase(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InspectPhase(ctx, InspectParams{WorkspaceRoot: ws, Name: "bp", Phase: 2, Verdict: "signoff", AcceptanceIDs: []string{"a1"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestAcceptance(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RecordVerdict(ctx, VerdictParams{WorkspaceRoot: ws, Name: "bp", Verdict: "changes", Via: "cli", Notes: "one more"}); err != nil || n != 3 {
		t.Fatalf("second follow-up = %d (%v)", n, err)
	}
	rms, _ = st.ListRoadmaps(ctx, ws, "bp")
	if got := rms[len(rms)-1].Phases[2].Title; got != "Review fixes (round 2)" {
		t.Fatalf("round 2 title = %q", got)
	}
}

func TestRejectionsDontCountRounds(t *testing.T) {
	ctx := context.Background()
	st, ws := signedOff(t, "bp")
	if err := st.RequestAcceptance(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordVerdict(ctx, VerdictParams{WorkspaceRoot: ws, Name: "bp", Verdict: "changes", Via: "cli", Notes: "x"}); err != nil {
		t.Fatal(err)
	}
	bp, _ := st.GetBlueprint(ctx, ws, "bp")
	if bp.Phases[0].Rounds != 0 || bp.Status == BlueprintBlocked {
		t.Fatalf("rounds = %d status = %s", bp.Phases[0].Rounds, bp.Status)
	}
}

func TestRecordLanding(t *testing.T) {
	ctx := context.Background()
	st, ws := signedOff(t, "bp")
	land := LandingParams{WorkspaceRoot: ws, Name: "bp", Method: "pr", Ref: "https://example.test/pr/1", Via: "chat", Caller: "arch", Quote: "open a pr"}
	if err := st.RecordLanding(ctx, land); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("not accepted: err = %v", err)
	}
	if err := st.RequestAcceptance(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordVerdict(ctx, VerdictParams{WorkspaceRoot: ws, Name: "bp", Verdict: "accept", Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	bad := land
	bad.Method = "rebase"
	if err := st.RecordLanding(ctx, bad); !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("bad method: err = %v", err)
	}
	if err := st.RecordLanding(ctx, land); !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("no human prompt: err = %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := st.RecordPrompt(ctx, ws, "arch", "Open a PR please"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordLanding(ctx, land); err != nil {
		t.Fatal(err)
	}
	bp, _ := st.GetBlueprint(ctx, ws, "bp")
	if bp.Status != BlueprintHandover || bp.LandingMethod != "pr" || bp.LandingRef != land.Ref || bp.LandedAt == nil {
		t.Fatalf("bp = %+v", bp)
	}
}
