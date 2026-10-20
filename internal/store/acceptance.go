package store

// This file implements the human's acceptance of a finished blueprint: the
// architect asks, the human accepts (then picks how the work lands) or asks
// for changes, which become a follow-up phase. Verdicts carry the same
// protection as roadmap approvals: a quote from a prompt the human typed
// after the request, or the human's own terminal.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// VerdictParams describes the human's verdict on finished work.
type VerdictParams struct {
	WorkspaceRoot string
	Name          string
	Verdict       string // "accept" | "changes"
	Via           string // "chat" | "cli"
	Caller        string // chat: the architect
	Quote         string // chat: the human's words
	Notes         string // changes: what to fix (becomes the follow-up phase)
	Title         string // changes: follow-up phase title ("" = "Review fixes (round N)")
}

// RequestAcceptance hands finished work to the human: every roadmap phase
// is specced and the last one is signed off.
func (s *Store) RequestAcceptance(ctx context.Context, ws, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, ws, name)
		if err != nil {
			return err
		}
		if bp.Status != BlueprintActive {
			return fmt.Errorf("%w: blueprint is %s, not active", ErrBadTransition, bp.Status)
		}
		rm, err := latestRoadmap(ctx, tx, ws, name)
		if err != nil {
			return err
		}
		if rm == nil || rm.ApprovedAt == nil || bp.CurrentPhase != len(rm.Phases) {
			return fmt.Errorf("%w: finish every roadmap phase first", ErrBadTransition)
		}
		ph, err := getPhase(ctx, tx, ws, name, bp.CurrentPhase)
		if err != nil {
			return err
		}
		if ph.Status != PhaseSignoff {
			return fmt.Errorf("%w: phase %d is %s, not signed off", ErrBadTransition, ph.Phase, ph.Status)
		}
		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx,
			`UPDATE blueprints SET status=?, accept_requested_at=?, updated_at=? WHERE workspace_root=? AND name=?`,
			BlueprintAcceptance, now, now, ws, name)
		return err
	})
}

// RecordVerdict records the human's review. Accept marks the blueprint
// accepted; changes adds an approved follow-up phase and reopens it. It
// returns the follow-up phase number (0 on accept).
func (s *Store) RecordVerdict(ctx context.Context, p VerdictParams) (int, error) {
	if p.Verdict != "accept" && p.Verdict != "changes" {
		return 0, fmt.Errorf("%w: verdict must be accept or changes", ErrInvalidBlueprint)
	}
	if p.Via != "chat" && p.Via != "cli" {
		return 0, fmt.Errorf("%w: via must be chat or cli", ErrInvalidBlueprint)
	}
	if p.Verdict == "changes" && strings.TrimSpace(p.Notes) == "" {
		return 0, fmt.Errorf("%w: say what needs fixing (notes)", ErrInvalidBlueprint)
	}
	followUp := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		if bp.Status != BlueprintAcceptance || bp.AcceptRequestedAt == nil {
			return fmt.Errorf("%w: blueprint is %s, not awaiting acceptance", ErrBadTransition, bp.Status)
		}
		quote := ""
		if p.Via == "chat" {
			quote = p.Quote
			if err := humanSaid(ctx, tx, p.WorkspaceRoot, p.Caller, quote, *bp.AcceptRequestedAt); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if p.Verdict == "accept" {
			_, err := tx.ExecContext(ctx,
				`UPDATE blueprints SET status=?, accepted_at=?, verdict_via=?, verdict_quote=?, updated_at=?
				 WHERE workspace_root=? AND name=?`,
				BlueprintAccepted, now, p.Via, quote, now, p.WorkspaceRoot, p.Name)
			return err
		}
		rm, err := latestRoadmap(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		round := bp.ReviewRounds + 1
		title := strings.TrimSpace(p.Title)
		if title == "" {
			title = fmt.Sprintf("Review fixes (round %d)", round)
		}
		followUp = len(rm.Phases) + 1
		phases := append(append([]RoadmapPhase{}, rm.Phases...), RoadmapPhase{Phase: followUp, Title: title, Delivers: p.Notes})
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO blueprint_roadmaps (workspace_root, name, version, phases_json, out_of_scope, proposed_at,
				approved_at, approved_via, approval_quote, base)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.WorkspaceRoot, p.Name, rm.Version+1, mustJSON(phases), rm.OutOfScope, now,
			now, p.Via, quote, rm.Base); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE blueprints SET status=?, review_rounds=?, accept_requested_at=NULL, verdict_via=?, verdict_quote=?, updated_at=?
			 WHERE workspace_root=? AND name=?`,
			BlueprintActive, round, p.Via, quote, now, p.WorkspaceRoot, p.Name)
		return err
	})
	return followUp, err
}

// LandingParams describes how accepted work landed.
type LandingParams struct {
	WorkspaceRoot string
	Name          string
	Method        string // "merge" | "branch" | "pr" | "manual"
	Ref           string // merge commit, branch, or PR URL
	Via           string // "chat" | "cli"
	Caller        string // chat: the architect
	Quote         string // chat: the human's words choosing the method
}

var landingMethods = map[string]bool{"merge": true, "branch": true, "pr": true, "manual": true}

// RecordLanding records how accepted work landed and closes the blueprint.
// The method is the human's choice: quoted from a prompt typed after the
// acceptance, or recorded from their terminal.
func (s *Store) RecordLanding(ctx context.Context, p LandingParams) error {
	if !landingMethods[p.Method] {
		return fmt.Errorf("%w: method must be merge, branch, pr, or manual", ErrInvalidBlueprint)
	}
	if p.Via != "chat" && p.Via != "cli" {
		return fmt.Errorf("%w: via must be chat or cli", ErrInvalidBlueprint)
	}
	if p.Method == "pr" && strings.TrimSpace(p.Ref) == "" {
		return fmt.Errorf("%w: a pr landing needs the PR URL as ref", ErrInvalidBlueprint)
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		if bp.Status != BlueprintAccepted || bp.AcceptedAt == nil {
			return fmt.Errorf("%w: blueprint is %s, not accepted", ErrBadTransition, bp.Status)
		}
		if p.Via == "chat" {
			if err := humanSaid(ctx, tx, p.WorkspaceRoot, p.Caller, p.Quote, *bp.AcceptedAt); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx,
			`UPDATE blueprints SET status=?, landing_method=?, landing_ref=?, landed_at=?, updated_at=?
			 WHERE workspace_root=? AND name=?`,
			BlueprintHandover, p.Method, p.Ref, now, now, p.WorkspaceRoot, p.Name)
		return err
	})
}
