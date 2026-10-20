package store

// This file implements series membership: builders joining a blueprint by
// name, and builders waiting for a series to open.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// JoinBlueprint makes builder the builder of a series that has none yet.
// If the builder was registered as a series waiter, it is removed atomically.
func (s *Store) JoinBlueprint(ctx context.Context, ws, name, builder string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, ws, name)
		if err != nil {
			return err
		}
		if err := writable(bp); err != nil {
			return err
		}
		if bp.Builder != "" {
			return fmt.Errorf("%w: %s already has builder %s", ErrBadTransition, name, bp.Builder)
		}
		if bp.Architect == builder {
			return fmt.Errorf("%w: the architect cannot also build %s", ErrBadTransition, name)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE blueprints SET builder=?, updated_at=? WHERE workspace_root=? AND name=?`,
			builder, time.Now().UTC(), ws, name)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM series_waiters WHERE workspace_root=? AND agent=?`,
			ws, builder)
		return err
	})
}

// ListOpenSeries returns series still waiting for a builder, by name.
func (s *Store) ListOpenSeries(ctx context.Context, ws string) ([]*Blueprint, error) {
	all, err := s.ListBlueprints(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := []*Blueprint{}
	for _, b := range all {
		if b.Series && b.Builder == "" && (b.Status == BlueprintDrafting || b.Status == BlueprintActive) {
			out = append(out, b)
		}
	}
	return out, nil
}

// LastBuilder returns the builder of architect's most recently updated
// blueprint, or "" (the team's builder, for pairing the next series).
func (s *Store) LastBuilder(ctx context.Context, ws, architect string) (string, error) {
	var b string
	err := s.db.QueryRowContext(ctx,
		`SELECT builder FROM blueprints WHERE workspace_root=? AND architect=? AND builder<>''
		 ORDER BY updated_at DESC, rowid DESC LIMIT 1`, ws, architect).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return b, err
}

// AddSeriesWaiter records an unassigned builder waiting for a series.
func (s *Store) AddSeriesWaiter(ctx context.Context, ws, agent string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO series_waiters (workspace_root, agent) VALUES (?, ?)`, ws, agent)
	return err
}

// RemoveSeriesWaiter forgets a waiter (idempotent).
func (s *Store) RemoveSeriesWaiter(ctx context.Context, ws, agent string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM series_waiters WHERE workspace_root=? AND agent=?`, ws, agent)
	return err
}

// ListSeriesWaiters returns the workspace's waiting builders.
func (s *Store) ListSeriesWaiters(ctx context.Context, ws string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent FROM series_waiters WHERE workspace_root=? ORDER BY agent`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetSeries records a series' worktree, branch and base commit.
func (s *Store) SetSeries(ctx context.Context, ws, name, worktree, branch, base string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE blueprints SET worktree_path=?, branch=?, base_ref=?, updated_at=? WHERE workspace_root=? AND name=?`,
		worktree, branch, base, time.Now().UTC(), ws, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoBlueprint
	}
	return nil
}

// SetAgentWorktree re-points an agent at a folder (a series worktree, or
// back to the main checkout once the series has landed).
func (s *Store) SetAgentWorktree(ctx context.Context, ws, agent, path string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET worktree_path=? WHERE workspace_root=? AND agent_id=?`, path, ws, agent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNoAgent, agent)
	}
	return nil
}
