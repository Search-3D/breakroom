package store

// This file implements blueprint roadmaps: versioned phase plans the human
// approves before any phase is specced.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const roadmapCols = `version, phases_json, out_of_scope, proposed_at, approved_at, approved_via, approval_quote, base`

// scanRoadmap reads one roadmaps row, decoding its phases.
func scanRoadmap(row interface{ Scan(...any) error }) (*Roadmap, error) {
	var r Roadmap
	var phasesJSON string
	var approved sql.NullTime
	if err := row.Scan(&r.Version, &phasesJSON, &r.OutOfScope, &r.ProposedAt,
		&approved, &r.ApprovedVia, &r.ApprovalQuote, &r.Base); err != nil {
		return nil, err
	}
	if approved.Valid {
		r.ApprovedAt = &approved.Time
	}
	if err := json.Unmarshal([]byte(phasesJSON), &r.Phases); err != nil {
		return nil, fmt.Errorf("store: roadmap phases: %w", err)
	}
	return &r, nil
}

// latestRoadmap returns the newest version, or nil when none was proposed.
func latestRoadmap(ctx context.Context, q querier, ws, name string) (*Roadmap, error) {
	r, err := scanRoadmap(q.QueryRowContext(ctx,
		`SELECT `+roadmapCols+` FROM blueprint_roadmaps
		 WHERE workspace_root=? AND name=? ORDER BY version DESC LIMIT 1`, ws, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// checkRoadmap enforces that a spec follows the approved latest roadmap.
func checkRoadmap(ctx context.Context, q querier, p SpecParams) error {
	rm, err := latestRoadmap(ctx, q, p.WorkspaceRoot, p.Name)
	if err != nil {
		return err
	}
	if rm == nil || rm.ApprovedAt == nil {
		return fmt.Errorf("%w: the human must approve the latest roadmap first", ErrRoadmapUnapproved)
	}
	if p.Phase > len(rm.Phases) {
		return fmt.Errorf("%w: roadmap v%d has %d phases; propose a new version to add phase %d",
			ErrInvalidBlueprint, rm.Version, len(rm.Phases), p.Phase)
	}
	if want := rm.Phases[p.Phase-1].Title; p.Title != want {
		return fmt.Errorf("%w: phase %d title must match the roadmap: %q", ErrInvalidBlueprint, p.Phase, want)
	}
	return nil
}

// ProposeRoadmap saves a new pending roadmap version. Phases already specced
// must keep their numbers and titles; base is the ref a series branches from.
func (s *Store) ProposeRoadmap(ctx context.Context, ws, name string, phases []RoadmapPhase, outOfScope, base string) (int, error) {
	if len(phases) == 0 {
		return 0, fmt.Errorf("%w: roadmap needs at least one phase", ErrInvalidBlueprint)
	}
	for i, ph := range phases {
		if ph.Phase != i+1 {
			return 0, fmt.Errorf("%w: phases must be numbered 1..%d in order", ErrInvalidBlueprint, len(phases))
		}
		if strings.TrimSpace(ph.Title) == "" {
			return 0, fmt.Errorf("%w: phase %d needs a title", ErrInvalidBlueprint, ph.Phase)
		}
	}
	var version int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, ws, name)
		if err != nil {
			return err
		}
		if err := writable(bp); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT phase, title FROM phases WHERE workspace_root=? AND blueprint_name=?`, ws, name)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n int
			var title string
			if err := rows.Scan(&n, &title); err != nil {
				return err
			}
			if n > len(phases) || phases[n-1].Title != title {
				return fmt.Errorf("%w: phase %d is already specced as %q and must stay", ErrInvalidBlueprint, n, title)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM blueprint_roadmaps WHERE workspace_root=? AND name=?`,
			ws, name).Scan(&version); err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO blueprint_roadmaps (workspace_root, name, version, phases_json, out_of_scope, proposed_at, base)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`, ws, name, version, mustJSON(phases), outOfScope, now, base); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE blueprints SET require_roadmap=1, updated_at=? WHERE workspace_root=? AND name=?`, now, ws, name)
		return err
	})
	return version, err
}

// ListRoadmaps returns every roadmap version of a blueprint, oldest first.
func (s *Store) ListRoadmaps(ctx context.Context, ws, name string) ([]Roadmap, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+roadmapCols+` FROM blueprint_roadmaps
		 WHERE workspace_root=? AND name=? ORDER BY version`, ws, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Roadmap{}
	for rows.Next() {
		r, err := scanRoadmap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
