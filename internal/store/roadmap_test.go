package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// approveForTest proposes a roadmap with these phase titles and approves it
// directly in the database, for tests that are about later phases.
func approveForTest(t *testing.T, st *Store, ws, name string, titles ...string) {
	t.Helper()
	phases := make([]RoadmapPhase, len(titles))
	for i, title := range titles {
		phases[i] = RoadmapPhase{Phase: i + 1, Title: title}
	}
	v, err := st.ProposeRoadmap(context.Background(), ws, name, phases, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE blueprint_roadmaps SET approved_at=?, approved_via='cli'
		WHERE workspace_root=? AND name=? AND version=?`, time.Now().UTC(), ws, name, v); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateV2ToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 2; v++ {
		if err := applyMigration(db, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blueprints (workspace_root, name, architect, builder, goal, status, current_phase)
		VALUES ('/ws', 'old', 'agent-1', 'agent-2', 'g', 'drafting', 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	bp, err := st.GetBlueprint(ctx, "/ws", "old")
	if err != nil {
		t.Fatal(err)
	}
	if bp.RequireRoadmap {
		t.Fatal("legacy blueprint must not require a roadmap")
	}
	if err := st.SpecPhase(ctx, testSpec("/ws", "old", 1)); err != nil {
		t.Fatalf("legacy spec without roadmap: %v", err)
	}
}

func TestSpecNeedsApprovedRoadmap(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", "goal"); err != nil {
		t.Fatal(err)
	}
	bp, err := st.GetBlueprint(ctx, ws, "bp")
	if err != nil {
		t.Fatal(err)
	}
	if !bp.RequireRoadmap {
		t.Fatal("new blueprint must require a roadmap")
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); !errors.Is(err, ErrRoadmapUnapproved) {
		t.Fatalf("spec without roadmap = %v, want ErrRoadmapUnapproved", err)
	}
	v, err := st.ProposeRoadmap(ctx, ws, "bp", []RoadmapPhase{{Phase: 1, Title: "phase"}}, "", "")
	if err != nil || v != 1 {
		t.Fatalf("propose = %d, %v", v, err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); !errors.Is(err, ErrRoadmapUnapproved) {
		t.Fatalf("spec with pending roadmap = %v, want ErrRoadmapUnapproved", err)
	}
	if _, err := st.db.Exec(`UPDATE blueprint_roadmaps SET approved_at=? WHERE name='bp'`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	wrong := testSpec(ws, "bp", 1)
	wrong.Title = "other"
	if err := st.SpecPhase(ctx, wrong); !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("spec with mismatched title = %v, want ErrInvalidBlueprint", err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); err != nil {
		t.Fatalf("spec after approval: %v", err)
	}
	if err := st.SubmitPhase(ctx, testSubmit(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InspectPhase(ctx, InspectParams{WorkspaceRoot: ws, Name: "bp", Phase: 1,
		Verdict: "signoff", AcceptanceIDs: []string{"a1"}, MaxRounds: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 2)); !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("spec beyond roadmap = %v, want ErrInvalidBlueprint", err)
	}
}

func TestProposeRoadmapRules(t *testing.T) {
	ctx := context.Background()
	st := openBlueprintStore(t)
	ws := "/ws"
	if err := st.InitBlueprint(ctx, ws, "bp", "agent-1", "agent-2", "goal"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		phases []RoadmapPhase
	}{
		{"empty", nil},
		{"phase gap", []RoadmapPhase{{Phase: 1, Title: "a"}, {Phase: 3, Title: "b"}}},
		{"blank title", []RoadmapPhase{{Phase: 1, Title: " "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.ProposeRoadmap(ctx, ws, "bp", tc.phases, "", ""); !errors.Is(err, ErrInvalidBlueprint) {
				t.Fatalf("propose %v = %v, want ErrInvalidBlueprint", tc.phases, err)
			}
		})
	}
	approveForTest(t, st, ws, "bp", "phase", "two")
	if err := st.SpecPhase(ctx, testSpec(ws, "bp", 1)); err != nil {
		t.Fatal(err)
	}
	renamed := []RoadmapPhase{{Phase: 1, Title: "renamed"}, {Phase: 2, Title: "two"}}
	if _, err := st.ProposeRoadmap(ctx, ws, "bp", renamed, "", ""); !errors.Is(err, ErrInvalidBlueprint) {
		t.Fatalf("renaming a specced phase = %v, want ErrInvalidBlueprint", err)
	}
	v, err := st.ProposeRoadmap(ctx, ws, "bp", []RoadmapPhase{{Phase: 1, Title: "phase"}, {Phase: 2, Title: "three"}}, "no docs", "")
	if err != nil || v != 2 {
		t.Fatalf("second proposal = %d, %v", v, err)
	}
	rms, err := st.ListRoadmaps(ctx, ws, "bp")
	if err != nil {
		t.Fatal(err)
	}
	if len(rms) != 2 || rms[0].Version != 1 || rms[1].Version != 2 || rms[1].OutOfScope != "no docs" || rms[1].ApprovedAt != nil {
		t.Fatalf("roadmaps = %+v", rms)
	}

	if err := st.InitBlueprint(ctx, ws, "closed", "agent-1", "agent-2", "g"); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseBlueprint(ctx, ws, "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeRoadmap(ctx, ws, "closed", []RoadmapPhase{{Phase: 1, Title: "a"}}, "", ""); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("propose on closed = %v, want ErrBadTransition", err)
	}

	if _, err := st.db.Exec(`INSERT INTO blueprints (workspace_root, name, architect, builder, status)
		VALUES (?, 'legacy', 'agent-1', 'agent-2', 'drafting')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeRoadmap(ctx, ws, "legacy", []RoadmapPhase{{Phase: 1, Title: "a"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if bp, _ := st.GetBlueprint(ctx, ws, "legacy"); !bp.RequireRoadmap {
		t.Fatal("proposing on a legacy blueprint must enable the roadmap rule")
	}
}

// A migration that fails partway leaves nothing behind, so the next start
// can retry it instead of tripping over half-applied DDL.
func TestMigrationIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "half.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 2; v++ {
		if err := applyMigration(db, v); err != nil {
			t.Fatal(err)
		}
	}
	// v3's CREATE TABLE human_prompts will collide after its ALTER ran.
	for _, stmt := range []string{`PRAGMA user_version=2`, `CREATE TABLE human_prompts (x INTEGER)`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if st, err := Open(path); err == nil {
		st.Close()
		t.Fatal("migration over a colliding table succeeded")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE human_prompts`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("retry after failed migration: %v", err)
	}
	st.Close()
}
