package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// openBlueprintStore opens a store on a fresh temp database.
func openBlueprintStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "bp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// testSpec is a valid two-requirement phase spec.
func testSpec(ws, name string, phase int) SpecParams {
	return SpecParams{
		WorkspaceRoot: ws, Name: name, Phase: phase,
		Title: "phase", Objective: "do it",
		Requirements: []Requirement{{ID: "r1", Text: "req one"}, {ID: "r2", Text: "req two"}},
		Acceptance:   []Requirement{{ID: "a1", Text: "acc one"}},
	}
}

// testSubmit is a valid deliverable for testSpec.
func testSubmit(ws, name string, phase int) SubmitParams {
	return SubmitParams{
		WorkspaceRoot: ws, Name: name, Phase: phase,
		Items: []DeliverableItem{
			{RequirementID: "r1", Summary: "did one"},
			{RequirementID: "r2", Summary: "did two"},
		},
		Proof: "ok 1.2s",
	}
}

func TestBlueprintLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", "goal"); err != nil {
		t.Fatal(err)
	}
	approveForTest(t, st, ws, "bp", "phase", "phase")
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	rounds, blocked, err := st.InspectPhase(ctx, InspectParams{
		WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "punchlist",
		Findings:  []Finding{{RequirementID: "r1", Issue: "gap", Fix: "fill"}},
		MaxRounds: 5,
	})
	if err != nil || blocked || rounds != 1 {
		t.Fatalf("punch = %d %v %v", rounds, blocked, err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	rounds, blocked, err = st.InspectPhase(ctx, InspectParams{
		WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "signoff",
		AcceptanceIDs: []string{"a1"}, MaxRounds: 5,
	})
	if err != nil || blocked || rounds != 1 {
		t.Fatalf("signoff = %d %v %v", rounds, blocked, err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 2)); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InspectPhase(ctx, InspectParams{
		WorkspaceRoot: ws, Name: "bp", Phase: 2, Verdict: "signoff",
		AcceptanceIDs: []string{"a1"}, MaxRounds: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseBlueprint(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	bp, err := st.GetBlueprint(ctx, ws, "bp")
	if err != nil {
		t.Fatal(err)
	}
	if bp.Status != BlueprintHandover || bp.CurrentPhase != 2 || len(bp.Phases) != 2 {
		t.Fatalf("bp = %+v", bp)
	}
	if bp.Phases[0].Status != PhaseSignoff || len(bp.Phases[0].Inspections) != 2 {
		t.Fatalf("phase1 = %+v", bp.Phases[0])
	}
	if bp.Phases[0].Deliverable == nil || bp.Phases[0].Deliverable.Proof != "ok 1.2s" {
		t.Fatalf("deliverable = %+v", bp.Phases[0].Deliverable)
	}
	list, err := st.ListBlueprints(ctx, ws)
	if err != nil || len(list) != 1 || list[0].Status != BlueprintHandover {
		t.Fatalf("list = %+v %v", list, err)
	}
}

func TestBlueprintTransitions(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", ""); err != nil {
		t.Fatal(err)
	}
	approveForTest(t, st, ws, "bp", "phase", "phase")
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", ""); !errors.Is(err, ErrBlueprintExists) {
		t.Fatalf("dup init = %v, want ErrBlueprintExists", err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 2)); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("spec phase 2 first = %v, want ErrBadTransition", err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("submit before spec = %v, want ErrBadTransition", err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 2)); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("spec phase 2 before phase 1 signoff = %v, want ErrBadTransition", err)
	}
	if _, _, err := st.InspectPhase(ctx, InspectParams{
		WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "signoff",
		AcceptanceIDs: []string{"a1"}, MaxRounds: 5,
	}); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("inspect before submit = %v, want ErrBadTransition", err)
	}
	if err := st.CloseBlueprint(ctx, ws, "bp"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("close active = %v, want ErrBadTransition", err)
	}
	if _, err := st.GetBlueprint(ctx, ws, "nope"); !errors.Is(err, ErrNoBlueprint) {
		t.Fatalf("get missing = %v, want ErrNoBlueprint", err)
	}
}

func TestBlueprintValidation(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", ""); err != nil {
		t.Fatal(err)
	}
	approveForTest(t, st, ws, "bp", "phase", "phase")
	bad := testSpec(ws, "bp", 1)
	bad.Requirements = []Requirement{{ID: "r1", Text: "x"}, {ID: "r1", Text: "y"}}
	if err := st.SpecPhase(ctx, bad); !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("dup ids = %v, want ErrInvalidBlueprint", err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		modify func(*SubmitParams)
	}{
		{"partial coverage", func(s *SubmitParams) { s.Items = s.Items[:1] }},
		{"empty proof", func(s *SubmitParams) { s.Proof = "  " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := testSubmit(ws, "bp", 1)
			tc.modify(&sub)
			if err := st.SubmitPhase(ctx, sub); !errors.Is(err, ErrInvalidBlueprint) {
				t.Fatalf("%s = %v, want ErrInvalidBlueprint", tc.name, err)
			}
		})
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		params InspectParams
	}{
		{
			name:   "signoff uncited",
			params: InspectParams{WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "signoff", MaxRounds: 5},
		},
		{
			name:   "punch bad id",
			params: InspectParams{WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "punchlist", Findings: []Finding{{RequirementID: "rx", Issue: "i", Fix: "f"}}, MaxRounds: 5},
		},
		{
			name:   "bad verdict",
			params: InspectParams{WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "maybe", MaxRounds: 5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := st.InspectPhase(ctx, tc.params); !errors.Is(err, ErrInvalidBlueprint) {
				t.Fatalf("%s = %v, want ErrInvalidBlueprint", tc.name, err)
			}
		})
	}
}

func TestBlueprintRoundCap(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", ""); err != nil {
		t.Fatal(err)
	}
	approveForTest(t, st, ws, "bp", "phase", "phase")
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	punch := func() (int, bool, error) {
		if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); err != nil {
			return 0, false, err
		}
		return st.InspectPhase(ctx, InspectParams{
			WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "punchlist",
			Findings:  []Finding{{RequirementID: "r1", Issue: "gap", Fix: "fill"}},
			MaxRounds: 2,
		})
	}
	if rounds, blocked, err := punch(); err != nil || blocked || rounds != 1 {
		t.Fatalf("punch1 = %d %v %v", rounds, blocked, err)
	}
	if rounds, blocked, err := punch(); err != nil || blocked || rounds != 2 {
		t.Fatalf("punch2 = %d %v %v", rounds, blocked, err)
	}
	if rounds, blocked, err := punch(); err != nil || !blocked || rounds != 3 {
		t.Fatalf("punch3 = %d %v %v, want blocked", rounds, blocked, err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); !errors.Is(err, ErrBlueprintBlocked) {
		t.Fatalf("submit blocked = %v, want ErrBlueprintBlocked", err)
	}
	if err := st.CloseBlueprint(ctx, ws, "bp"); err != nil {
		t.Fatal(err)
	}
	bp, err := st.GetBlueprint(ctx, ws, "bp")
	if err != nil || bp.Status != BlueprintHandover {
		t.Fatalf("bp = %+v %v", bp, err)
	}
}

func TestBlueprintSelfPairRejected(t *testing.T) {
	st := openBlueprintStore(t)
	err := st.InitBlueprint(context.Background(), "/ws", "bp", "agent-1", "agent-1", "")
	if !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("self-pair err = %v, want ErrInvalidBlueprint", err)
	}
}

// Concurrent verdicts on one deliverable: exactly one may win; the loser
// sees the phase already moved on.
func TestBlueprintConcurrentInspect(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", ""); err != nil {
		t.Fatal(err)
	}
	approveForTest(t, st, ws, "bp", "phase", "phase")
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	punch := InspectParams{
		WorkspaceRoot: ws, Name: "bp", Phase: 1, Verdict: "punchlist",
		Findings:  []Finding{{RequirementID: "r1", Issue: "gap", Fix: "fill"}},
		MaxRounds: 100,
	}
	for trial := range 30 {
		if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); err != nil {
			t.Fatalf("trial %d submit: %v", trial, err)
		}
		errs := make(chan error, 2)
		for range 2 {
			go func() { _, _, err := st.InspectPhase(ctx, punch); errs <- err }()
		}
		e1, e2 := <-errs, <-errs
		if (e1 == nil) == (e2 == nil) {
			t.Fatalf("trial %d: errs = %v, %v; want exactly one winner", trial, e1, e2)
		}
		if e := errors.Join(e1, e2); !errors.Is(e, ErrBadTransition) {
			t.Fatalf("trial %d: loser err = %v, want ErrBadTransition", trial, e)
		}
	}
	bp, err := st.GetBlueprint(ctx, ws, "bp")
	if err != nil || bp.Phases[0].Rounds != 30 || len(bp.Phases[0].Inspections) != 30 {
		t.Fatalf("rounds = %d inspections = %d, want 30/30 (%v)",
			bp.Phases[0].Rounds, len(bp.Phases[0].Inspections), err)
	}
}

func TestInitWithoutBuilder(t *testing.T) {
	s, ctx := openTest(t)
	if err := s.InitBlueprint(ctx, "/ws", "api-debugging", "arch", "", "g"); err != nil {
		t.Fatal(err)
	}
	bp, err := s.GetBlueprint(ctx, "/ws", "api-debugging")
	if err != nil || !bp.Series || bp.Builder != "" {
		t.Fatalf("bp = %+v (%v), want a series with no builder", bp, err)
	}
	if err := s.InitBlueprint(ctx, "/ws", "Bad Name", "arch", "", "g"); !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("bad series name: err = %v, want ErrInvalidBlueprint", err)
	}
	if err := s.InitBlueprint(ctx, "/ws", "Manual Name", "arch", "b1", "g"); err != nil {
		t.Fatal(err)
	}
	if bp, _ := s.GetBlueprint(ctx, "/ws", "Manual Name"); bp.Series {
		t.Fatal("a blueprint with a builder is not a series")
	}
}

func TestJoinBlueprint(t *testing.T) {
	s, ctx := openTest(t)
	for _, n := range []string{"s1", "s2", "s3"} {
		if err := s.InitBlueprint(ctx, "/ws", n, "arch", "", "g"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.JoinBlueprint(ctx, "/ws", "s1", "arch"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("architect joining own: %v", err)
	}
	if err := s.AddSeriesWaiter(ctx, "/ws", "b1"); err != nil {
		t.Fatal(err)
	}
	waiters, _ := s.ListSeriesWaiters(ctx, "/ws")
	if len(waiters) != 1 || waiters[0] != "b1" {
		t.Fatalf("waiters before join = %v", waiters)
	}
	if err := s.JoinBlueprint(ctx, "/ws", "s1", "b1"); err != nil {
		t.Fatal(err)
	}
	waiters, _ = s.ListSeriesWaiters(ctx, "/ws")
	if len(waiters) != 0 {
		t.Fatalf("waiters after join = %v, want empty", waiters)
	}
	if err := s.JoinBlueprint(ctx, "/ws", "s1", "b2"); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("second join: %v", err)
	}
	if bp, _ := s.GetBlueprint(ctx, "/ws", "s1"); bp.Builder != "b1" {
		t.Fatalf("builder = %q", bp.Builder)
	}
	if err := s.CloseBlueprint(ctx, "/ws", "s3"); err != nil {
		t.Fatal(err)
	}
	open, err := s.ListOpenSeries(ctx, "/ws")
	if err != nil || len(open) != 1 || open[0].Name != "s2" {
		t.Fatalf("open = %v (%v), want only s2", open, err)
	}
}
