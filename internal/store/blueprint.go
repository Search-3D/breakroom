// This file implements Blueprint persistence: the Architect/Builder
// stage-gate loop from docs/blueprint-spec.md. Sequencing and shape are
// enforced here; roles and notifications live in internal/daemon.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/search-3d/breakroom/internal/protocol"
)

// Blueprint lifecycle states.
const (
	BlueprintDrafting = "drafting"
	BlueprintActive   = "active"
	BlueprintBlocked  = "blocked"
	BlueprintHandover = "handover"
	// BlueprintAcceptance waits on the human to accept the finished work
	// or ask for changes; BlueprintAccepted waits on how it lands.
	BlueprintAcceptance = "acceptance"
	BlueprintAccepted   = "accepted"
)

// Phase lifecycle states.
const (
	PhaseConstructing = "constructing"
	PhaseInspection   = "inspection"
	PhasePunchlist    = "punchlist"
	PhaseSignoff      = "signoff"
)

var (
	// ErrNoBlueprint is returned when a blueprint name is unknown.
	ErrNoBlueprint = errors.New("no such blueprint")
	// ErrBlueprintExists is returned when a blueprint name is taken.
	ErrBlueprintExists = errors.New("blueprint already exists")
	// ErrBadTransition is returned when a call mismatches current state.
	ErrBadTransition = errors.New("bad blueprint transition")
	// ErrBlueprintBlocked is returned when a blocked blueprint is written.
	ErrBlueprintBlocked = errors.New("blueprint is blocked")
	// ErrRoadmapUnapproved is returned when a phase is specced before the
	// human approved the latest roadmap.
	ErrRoadmapUnapproved = errors.New("roadmap not approved")
	// ErrHumanApprovalRequired is returned when a chat approval is not backed
	// by a prompt the human typed in the architect's session.
	ErrHumanApprovalRequired = errors.New("human approval required")
	// ErrInvalidBlueprint is returned for malformed payloads (bad ids,
	// coverage gaps, empty proof).
	ErrInvalidBlueprint = errors.New("invalid blueprint payload")
)

// The spec, deliverable and finding shapes are the wire types: they are
// stored as the JSON the API sends.
type (
	Requirement     = protocol.BlueprintRequirement
	DeliverableItem = protocol.BlueprintItem
	Deliverable     = protocol.BlueprintDeliverable
	Finding         = protocol.BlueprintFinding
	RoadmapPhase    = protocol.RoadmapPhase
)

// Inspection is one recorded verdict on a phase.
type Inspection struct {
	Verdict       string    `json:"verdict"` // signoff|punchlist
	AcceptanceIDs []string  `json:"acceptance_ids,omitempty"`
	Findings      []Finding `json:"findings,omitempty"`
	At            time.Time `json:"at"`
}

// Phase is one blueprint phase with its spec, deliverable, and inspections.
type Phase struct {
	Phase        int
	Title        string
	Objective    string
	Requirements []Requirement
	Acceptance   []Requirement
	Constraints  string
	Status       string
	Rounds       int
	Deliverable  *Deliverable
	Inspections  []Inspection
	UpdatedAt    time.Time
}

// Blueprint is one named Architect/Builder run with its phases in order.
type Blueprint struct {
	WorkspaceRoot string
	Name          string
	Architect     string
	Builder       string
	Goal          string
	Status        string
	CurrentPhase  int
	// RequireRoadmap gates specs on an approved roadmap. Blueprints created
	// before roadmaps existed keep false until they propose one.
	RequireRoadmap bool
	// Series marks a blueprint created without a builder: the builder joins
	// by name and Breakroom manages its worktree.
	Series bool
	// WorktreePath, Branch and BaseRef describe a series' worktree once
	// created (empty before, and for blueprints that aren't series).
	WorktreePath string
	Branch       string
	BaseRef      string
	// Acceptance and landing (see acceptance.go).
	AcceptRequestedAt *time.Time
	AcceptedAt        *time.Time
	ReviewRounds      int
	LandingMethod     string
	LandingRef        string
	LandedAt          *time.Time
	Phases            []Phase
}

// Roadmap is one proposed version of a blueprint's phase plan.
type Roadmap struct {
	Version       int
	Phases        []RoadmapPhase
	OutOfScope    string
	Base          string // ref a series should branch from ("" = main checkout HEAD)
	ProposedAt    time.Time
	ApprovedAt    *time.Time
	ApprovedVia   string // chat|cli, empty while pending
	ApprovalQuote string
}

// blueprintCols are the columns scanBlueprint reads, in order.
const blueprintCols = `workspace_root, name, architect, builder, goal, status,
	current_phase, created_at, updated_at, require_roadmap, series,
	worktree_path, branch, base_ref,
	accept_requested_at, accepted_at, review_rounds, landing_method, landing_ref, landed_at`

// scanBlueprint reads one blueprints row.
func scanBlueprint(row interface{ Scan(...any) error }) (*Blueprint, error) {
	var b Blueprint
	var created, updated time.Time
	var requested, accepted, landed sql.NullTime
	err := row.Scan(&b.WorkspaceRoot, &b.Name, &b.Architect, &b.Builder,
		&b.Goal, &b.Status, &b.CurrentPhase, &created, &updated, &b.RequireRoadmap, &b.Series,
		&b.WorktreePath, &b.Branch, &b.BaseRef,
		&requested, &accepted, &b.ReviewRounds, &b.LandingMethod, &b.LandingRef, &landed)
	if err != nil {
		return nil, err
	}
	for _, t := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{requested, &b.AcceptRequestedAt}, {accepted, &b.AcceptedAt}, {landed, &b.LandedAt}} {
		if t.src.Valid {
			v := t.src.Time
			*t.dst = &v
		}
	}
	b.Phases = []Phase{}
	return &b, nil
}

// scanPhase reads one phases row, decoding its JSON columns.
func scanPhase(row interface{ Scan(...any) error }) (*Phase, error) {
	var p Phase
	var ws, name, reqJSON, accJSON, delJSON, inspJSON string
	err := row.Scan(&ws, &name, &p.Phase, &p.Title, &p.Objective,
		&reqJSON, &accJSON, &p.Constraints, &p.Status, &p.Rounds,
		&delJSON, &inspJSON, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.Requirements = []Requirement{}
	if err := json.Unmarshal([]byte(reqJSON), &p.Requirements); err != nil {
		return nil, fmt.Errorf("store: bad requirements_json: %w", err)
	}
	p.Acceptance = []Requirement{}
	if err := json.Unmarshal([]byte(accJSON), &p.Acceptance); err != nil {
		return nil, fmt.Errorf("store: bad acceptance_json: %w", err)
	}
	if delJSON != "" {
		var d Deliverable
		if err := json.Unmarshal([]byte(delJSON), &d); err != nil {
			return nil, fmt.Errorf("store: bad deliverable_json: %w", err)
		}
		p.Deliverable = &d
	}
	p.Inspections = []Inspection{}
	if err := json.Unmarshal([]byte(inspJSON), &p.Inspections); err != nil {
		return nil, fmt.Errorf("store: bad inspections_json: %w", err)
	}
	return &p, nil
}

// mustJSON marshals v, panicking on failure (only plain structs are passed).
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("store: marshal: %v", err))
	}
	return string(data)
}

// checkIDs requires every requirement to have a non-empty, unique id and text.
func checkIDs(reqs []Requirement) error {
	seen := map[string]bool{}
	for _, r := range reqs {
		if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Text) == "" {
			return fmt.Errorf("%w: ids and texts must be non-empty", ErrInvalidBlueprint)
		}
		if seen[r.ID] {
			return fmt.Errorf("%w: duplicate id %q", ErrInvalidBlueprint, r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

// querier is the slice of *sql.DB / *sql.Tx the blueprint reads need.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// inTx runs a blueprint change in one transaction. The pool has one connection,
// so changes run one at a time; fn must use tx, never s.db, or it deadlocks.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// getBlueprint loads a blueprint, or returns ErrNoBlueprint.
func getBlueprint(ctx context.Context, q querier, ws, name string) (*Blueprint, error) {
	b, err := scanBlueprint(q.QueryRowContext(ctx,
		`SELECT `+blueprintCols+` FROM blueprints WHERE workspace_root=? AND name=?`, ws, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoBlueprint
	}
	return b, err
}

// getPhase loads one phase of a blueprint.
func getPhase(ctx context.Context, q querier, ws, name string, phase int) (*Phase, error) {
	p, err := scanPhase(q.QueryRowContext(ctx,
		`SELECT workspace_root, blueprint_name, phase, title, objective,
			requirements_json, acceptance_json, constraints, status, rounds,
			deliverable_json, inspections_json, updated_at
		 FROM phases WHERE workspace_root=? AND blueprint_name=? AND phase=?`,
		ws, name, phase))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no phase %d", ErrBadTransition, phase)
	}
	return p, err
}

// writable returns an error if the blueprint is blocked, closed, or awaiting
// the human, where builders and architects can't change it.
func writable(bp *Blueprint) error {
	if bp.Status == BlueprintBlocked {
		return ErrBlueprintBlocked
	}
	if bp.Status == BlueprintHandover {
		return fmt.Errorf("%w: blueprint is closed", ErrBadTransition)
	}
	if bp.Status == BlueprintAcceptance || bp.Status == BlueprintAccepted {
		return fmt.Errorf("%w: blueprint is with the human (%s)", ErrBadTransition, bp.Status)
	}
	return nil
}

// seriesName is what a series name must look like: it names a branch
// (breakroom/<name>) and a folder (.workspaces/<name>).
var seriesName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ErrBadSeriesName is returned when a series name can't be a branch/folder.
var ErrBadSeriesName = fmt.Errorf("%w: series names are lowercase letters, digits and dashes (max 48)", ErrInvalidBlueprint)

// InitBlueprint starts a drafting blueprint. Names are unique per workspace;
// architect and builder must be different agents. An empty builder starts a
// series: the builder joins later by name.
func (s *Store) InitBlueprint(ctx context.Context, ws, name, architect, builder, goal string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidBlueprint)
	}
	series := builder == ""
	if series && !seriesName.MatchString(name) {
		return ErrBadSeriesName
	}
	if architect == builder {
		return fmt.Errorf("%w: architect and builder must be different agents", ErrInvalidBlueprint)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := getBlueprint(ctx, tx, ws, name); err == nil {
			return ErrBlueprintExists
		} else if !errors.Is(err, ErrNoBlueprint) {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO blueprints (workspace_root, name, architect, builder, goal, status, current_phase, require_roadmap, series)
			 VALUES (?, ?, ?, ?, ?, 'drafting', 0, 1, ?)`,
			ws, name, architect, builder, goal, series)
		return err
	})
}

// SpecParams describes one phase spec from the architect.
type SpecParams struct {
	WorkspaceRoot string
	Name          string
	Phase         int
	Title         string
	Objective     string
	Requirements  []Requirement
	Acceptance    []Requirement
	Constraints   string
}

// SpecPhase records the next phase spec, creating the phase already
// constructing. The previous phase must be signed off (current+1 only).
func (s *Store) SpecPhase(ctx context.Context, p SpecParams) error {
	if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Objective) == "" {
		return fmt.Errorf("%w: title and objective are required", ErrInvalidBlueprint)
	}
	if len(p.Requirements) == 0 || len(p.Acceptance) == 0 {
		return fmt.Errorf("%w: requirements and acceptance must be non-empty", ErrInvalidBlueprint)
	}
	if err := checkIDs(p.Requirements); err != nil {
		return err
	}
	if err := checkIDs(p.Acceptance); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		if err := writable(bp); err != nil {
			return err
		}
		if p.Phase != bp.CurrentPhase+1 || p.Phase < 1 {
			return fmt.Errorf("%w: want phase %d", ErrBadTransition, bp.CurrentPhase+1)
		}
		if bp.CurrentPhase > 0 {
			prev, err := getPhase(ctx, tx, p.WorkspaceRoot, p.Name, bp.CurrentPhase)
			if err != nil {
				return err
			}
			if prev.Status != PhaseSignoff {
				return fmt.Errorf("%w: phase %d is %s", ErrBadTransition, prev.Phase, prev.Status)
			}
		}
		if bp.RequireRoadmap {
			if err := checkRoadmap(ctx, tx, p); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO phases (workspace_root, blueprint_name, phase, title, objective,
				requirements_json, acceptance_json, constraints, status, rounds,
				deliverable_json, inspections_json, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'constructing', 0, '', '[]', ?)`,
			p.WorkspaceRoot, p.Name, p.Phase, p.Title, p.Objective,
			mustJSON(p.Requirements), mustJSON(p.Acceptance), p.Constraints, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE blueprints SET status='active', current_phase=?, updated_at=?
			 WHERE workspace_root=? AND name=?`,
			p.Phase, now, p.WorkspaceRoot, p.Name)
		return err
	})
}

// SubmitParams describes one deliverable from the builder.
type SubmitParams struct {
	WorkspaceRoot string
	Name          string
	Phase         int
	Items         []DeliverableItem
	Proof         string
	Notes         string
}

// SubmitPhase records the builder's deliverable, moving the phase to
// inspection. Items must cover every requirement id.
func (s *Store) SubmitPhase(ctx context.Context, p SubmitParams) error {
	if strings.TrimSpace(p.Proof) == "" {
		return fmt.Errorf("%w: proof is required", ErrInvalidBlueprint)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		if err := writable(bp); err != nil {
			return err
		}
		ph, err := getPhase(ctx, tx, p.WorkspaceRoot, p.Name, p.Phase)
		if err != nil {
			return err
		}
		if p.Phase != bp.CurrentPhase || (ph.Status != PhaseConstructing && ph.Status != PhasePunchlist) {
			return fmt.Errorf("%w: phase %d is %s", ErrBadTransition, p.Phase, ph.Status)
		}
		known := map[string]bool{}
		for _, r := range ph.Requirements {
			known[r.ID] = false
		}
		for _, it := range p.Items {
			done, ok := known[it.RequirementID]
			if !ok {
				return fmt.Errorf("%w: unknown requirement %q", ErrInvalidBlueprint, it.RequirementID)
			}
			if !done && strings.TrimSpace(it.Summary) == "" {
				return fmt.Errorf("%w: empty summary for %q", ErrInvalidBlueprint, it.RequirementID)
			}
			known[it.RequirementID] = true
		}
		for id, done := range known {
			if !done {
				return fmt.Errorf("%w: requirement %q unmapped", ErrInvalidBlueprint, id)
			}
		}
		del := Deliverable{Items: p.Items, Proof: p.Proof, Notes: p.Notes}
		_, err = tx.ExecContext(ctx,
			`UPDATE phases SET status='inspection', deliverable_json=?, updated_at=?
			 WHERE workspace_root=? AND blueprint_name=? AND phase=?`,
			mustJSON(del), time.Now().UTC(), p.WorkspaceRoot, p.Name, p.Phase)
		return err
	})
}

// InspectParams describes one architect verdict on a phase.
type InspectParams struct {
	WorkspaceRoot string
	Name          string
	Phase         int
	Verdict       string // signoff|punchlist
	AcceptanceIDs []string
	Findings      []Finding
	MaxRounds     int
}

// InspectPhase records the verdict. Sign-off needs full acceptance coverage;
// a punch list needs valid findings and consumes one round; past MaxRounds
// the blueprint blocks (terminal) instead.
func (s *Store) InspectPhase(ctx context.Context, p InspectParams) (rounds int, blocked bool, err error) {
	if p.Verdict != "signoff" && p.Verdict != "punchlist" {
		return 0, false, fmt.Errorf("%w: verdict must be signoff or punchlist", ErrInvalidBlueprint)
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		if err := writable(bp); err != nil {
			return err
		}
		ph, err := getPhase(ctx, tx, p.WorkspaceRoot, p.Name, p.Phase)
		if err != nil {
			return err
		}
		if p.Phase != bp.CurrentPhase || ph.Status != PhaseInspection {
			return fmt.Errorf("%w: phase %d is %s", ErrBadTransition, p.Phase, ph.Status)
		}
		insp := Inspection{Verdict: p.Verdict, At: time.Now().UTC()}
		now := insp.At
		rounds = ph.Rounds
		status := PhaseSignoff
		if p.Verdict == "signoff" {
			known := map[string]bool{}
			for _, a := range ph.Acceptance {
				known[a.ID] = false
			}
			for _, id := range p.AcceptanceIDs {
				if _, ok := known[id]; !ok {
					return fmt.Errorf("%w: unknown acceptance %q", ErrInvalidBlueprint, id)
				}
				known[id] = true
			}
			for id, done := range known {
				if !done {
					return fmt.Errorf("%w: acceptance %q uncited", ErrInvalidBlueprint, id)
				}
			}
			insp.AcceptanceIDs = p.AcceptanceIDs
		} else {
			if len(p.Findings) == 0 {
				return fmt.Errorf("%w: findings are required", ErrInvalidBlueprint)
			}
			known := map[string]bool{}
			for _, r := range ph.Requirements {
				known[r.ID] = true
			}
			for _, f := range p.Findings {
				if !known[f.RequirementID] {
					return fmt.Errorf("%w: unknown requirement %q", ErrInvalidBlueprint, f.RequirementID)
				}
				if strings.TrimSpace(f.Issue) == "" || strings.TrimSpace(f.Fix) == "" {
					return fmt.Errorf("%w: issue and fix are required", ErrInvalidBlueprint)
				}
			}
			insp.Findings = p.Findings
			rounds++
			status = PhasePunchlist
			blocked = rounds > p.MaxRounds
			if blocked {
				// The deliverable stays where it was judged; the blueprint
				// itself carries the blocked state.
				status = PhaseInspection
			}
		}
		ph.Inspections = append(ph.Inspections, insp)
		if _, err := tx.ExecContext(ctx,
			`UPDATE phases SET status=?, rounds=?, inspections_json=?, updated_at=?
			 WHERE workspace_root=? AND blueprint_name=? AND phase=?`,
			status, rounds, mustJSON(ph.Inspections), now, p.WorkspaceRoot, p.Name, p.Phase); err != nil {
			return err
		}
		if blocked {
			_, err = tx.ExecContext(ctx,
				`UPDATE blueprints SET status='blocked', updated_at=?
				 WHERE workspace_root=? AND name=?`,
				now, p.WorkspaceRoot, p.Name)
		}
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return rounds, blocked, nil
}

// CloseBlueprint records handover. Allowed from any status except while a
// phase sits constructing/inspection/punchlist; closing twice succeeds.
func (s *Store) CloseBlueprint(ctx context.Context, ws, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, ws, name)
		if err != nil {
			return err
		}
		if bp.Status == BlueprintHandover {
			return nil
		}
		if bp.Status != BlueprintBlocked {
			var open string
			err := tx.QueryRowContext(ctx,
				`SELECT status FROM phases
				 WHERE workspace_root=? AND blueprint_name=? AND status<>'signoff' LIMIT 1`,
				ws, name).Scan(&open)
			if err == nil {
				return fmt.Errorf("%w: a phase is still %s", ErrBadTransition, open)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE blueprints SET status='handover', updated_at=? WHERE workspace_root=? AND name=?`,
			time.Now().UTC(), ws, name)
		return err
	})
}

// GetBlueprint returns one blueprint with its phases in order.
func (s *Store) GetBlueprint(ctx context.Context, ws, name string) (*Blueprint, error) {
	bp, err := getBlueprint(ctx, s.db, ws, name)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_root, blueprint_name, phase, title, objective,
			requirements_json, acceptance_json, constraints, status, rounds,
			deliverable_json, inspections_json, updated_at
		 FROM phases WHERE workspace_root=? AND blueprint_name=? ORDER BY phase`,
		ws, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPhase(rows)
		if err != nil {
			return nil, err
		}
		bp.Phases = append(bp.Phases, *p)
	}
	return bp, rows.Err()
}

// ListBlueprints returns the workspace's blueprint headers (no phases).
func (s *Store) ListBlueprints(ctx context.Context, ws string) ([]*Blueprint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+blueprintCols+` FROM blueprints WHERE workspace_root=? ORDER BY name`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Blueprint{}
	for rows.Next() {
		b, err := scanBlueprint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
