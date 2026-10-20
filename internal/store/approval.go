package store

// This file records what the human typed (via the prompt hook) and approves
// roadmaps against it. A chat approval is accepted only when the architect's
// quote appears in a prompt the human typed in the architect's own session
// after the roadmap was proposed. Not a boundary against an agent forging
// hook calls from its own shell; it stops mistaken and injected approvals.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxPromptBytes   = 65536
	promptsPerAgent  = 50
	emissionLifetime = 24 * time.Hour
)

// ApproveParams describes one roadmap approval.
type ApproveParams struct {
	WorkspaceRoot string
	Name          string
	Version       int    // 0 = latest pending
	Via           string // "chat" | "cli"
	Caller        string // chat: the architect; the daemon checks the role
	Quote         string // chat: the human's words
}

// word is one lowercased word of a prompt and the clause it sits in.
type word struct {
	text   string
	clause int
}

// splitWords lowercases s into words (apostrophes and hyphens stay inside
// words, so "don't" is one), numbering clauses split at sentence punctuation
// and dashes.
func splitWords(s string) []word {
	var out []word
	var cur strings.Builder
	clause := 0
	flush := func() {
		if w := strings.Trim(cur.String(), "'’-"); w != "" {
			out = append(out, word{w, clause})
		}
		cur.Reset()
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case strings.ContainsRune(".,;:!?()\n—–", r):
			flush()
			clause++
		case unicode.IsSpace(r) || (unicode.IsPunct(r) && !strings.ContainsRune("'’-", r)):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// negations mark a clause that turns the quote around ("don't accept").
var negations = map[string]bool{
	"no": true, "not": true, "nope": true, "nah": true, "never": true, "without": true,
	"cannot": true, "dont": true, "cant": true, "wont": true, "isnt": true, "doesnt": true,
	"didnt": true, "shouldnt": true, "wouldnt": true, "arent": true,
}

// negation reports whether w negates a clause ("no", "not", "don't", ...).
func negation(w string) bool {
	return negations[w] || strings.HasSuffix(w, "n't") || strings.HasSuffix(w, "n’t")
}

// quoteBacked reports whether prompt contains quote as whole words, in
// clauses free of negations. A one-character quote ("1", "y") must be its
// whole clause, so a stray digit elsewhere in the prompt can't back it.
func quoteBacked(prompt string, quote []word) bool {
	p := splitWords(prompt)
	for i := 0; i+len(quote) <= len(p); i++ {
		match := true
		for j, q := range quote {
			if p[i+j].text != q.text {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		first, last := p[i].clause, p[i+len(quote)-1].clause
		clean, clauseLen := true, 0
		for _, w := range p {
			if w.clause >= first && w.clause <= last {
				clean = clean && !negation(w.text)
				clauseLen++
			}
		}
		short := len(quote) == 1 && utf8.RuneCountInString(quote[0].text) == 1
		if clean && (!short || clauseLen == 1) {
			return true
		}
	}
	return false
}

// emissionHash fingerprints text the gate sent, so it isn't later mistaken
// for something the human typed. Whitespace is normalized: a host re-posting
// it may rewrap lines or change line endings.
// ponytail: whitespace only; a host that rewrites the text otherwise slips past.
func emissionHash(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
}

// ApproveRoadmap approves the latest roadmap version. Chat approvals must be
// backed by a recorded human prompt; CLI approvals come from the human's own
// terminal and need none.
func (s *Store) ApproveRoadmap(ctx context.Context, p ApproveParams) (Roadmap, error) {
	if p.Via != "chat" && p.Via != "cli" {
		return Roadmap{}, fmt.Errorf("%w: via must be chat or cli", ErrInvalidBlueprint)
	}
	var out Roadmap
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		bp, err := getBlueprint(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		if err := writable(bp); err != nil {
			return err
		}
		rm, err := latestRoadmap(ctx, tx, p.WorkspaceRoot, p.Name)
		if err != nil {
			return err
		}
		if rm == nil || rm.ApprovedAt != nil {
			return fmt.Errorf("%w: no roadmap is pending approval", ErrBadTransition)
		}
		if p.Version != 0 && p.Version != rm.Version {
			return fmt.Errorf("%w: v%d is not the latest roadmap (v%d)", ErrBadTransition, p.Version, rm.Version)
		}
		quote := ""
		if p.Via == "chat" {
			quote = p.Quote
			if err := humanSaid(ctx, tx, p.WorkspaceRoot, p.Caller, quote, rm.ProposedAt); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`UPDATE blueprint_roadmaps SET approved_at=?, approved_via=?, approval_quote=?
			 WHERE workspace_root=? AND name=? AND version=?`,
			now, p.Via, quote, p.WorkspaceRoot, p.Name, rm.Version); err != nil {
			return err
		}
		rm.ApprovedAt, rm.ApprovedVia, rm.ApprovalQuote = &now, p.Via, quote
		out = *rm
		return nil
	})
	return out, err
}

// humanSaid checks that agent's session recorded a prompt at or after since
// containing quote.
func humanSaid(ctx context.Context, q querier, ws, agent, quote string, since time.Time) error {
	want := splitWords(quote)
	if len(want) == 0 {
		return fmt.Errorf("%w: quote the human's reply", ErrHumanApprovalRequired)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT text, created_at FROM human_prompts WHERE workspace_root=? AND agent=?`, ws, agent)
	if err != nil {
		return err
	}
	defer rows.Close()
	seenTotal := 0
	seenSince := 0
	for rows.Next() {
		seenTotal++
		var text string
		var at time.Time
		if err := rows.Scan(&text, &at); err != nil {
			return err
		}
		if !at.Before(since) {
			seenSince++
			if quoteBacked(text, want) {
				return nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if seenTotal == 0 {
		return fmt.Errorf("%w: no prompt recorded for %s; check that the host's prompt hook is installed (or approve/accept via breakroom CLI)", ErrHumanApprovalRequired, agent)
	}
	if seenSince == 0 {
		return fmt.Errorf("%w: no prompt from the human in %s's session since the request was proposed; reply in chat to confirm, or use breakroom CLI", ErrHumanApprovalRequired, agent)
	}
	return fmt.Errorf("%w: no prompt from the human in %s's session since the request contains that quote as whole words "+
		"outside a negation (no/not/don't); quote their reply exactly, or ask them to confirm", ErrHumanApprovalRequired, agent)
}

// RecordPrompt stores a prompt the human typed in agent's session. Text the
// stop gate emitted itself (hosts that continue by re-submitting it) is not
// recorded. Text beyond 64KB keeps its head and tail, where approvals sit.
func (s *Store) RecordPrompt(ctx context.Context, ws, agent, text string) (bool, error) {
	recorded := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT created_at FROM gate_emissions WHERE workspace_root=? AND agent=? AND hash=?`,
			ws, agent, emissionHash(text))
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-emissionLifetime)
		echo := false
		for rows.Next() {
			var at time.Time
			if err := rows.Scan(&at); err != nil {
				rows.Close()
				return err
			}
			echo = echo || at.After(cutoff)
		}
		rows.Close()
		if err := rows.Err(); err != nil || echo {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO human_prompts (workspace_root, agent, text, created_at) VALUES (?, ?, ?, ?)`,
			ws, agent, keepEnds(text, maxPromptBytes), time.Now().UTC()); err != nil {
			return err
		}
		recorded = true
		_, err = tx.ExecContext(ctx,
			`DELETE FROM human_prompts WHERE workspace_root=? AND agent=? AND id NOT IN (
				SELECT id FROM human_prompts WHERE workspace_root=? AND agent=? ORDER BY id DESC LIMIT ?)`,
			ws, agent, ws, agent, promptsPerAgent)
		return err
	})
	return recorded, err
}

// RecordEmission remembers a continuation text the stop gate sent agent, so
// a host that re-submits it as a user prompt is not mistaken for the human.
func (s *Store) RecordEmission(ctx context.Context, ws, agent, text string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO gate_emissions (workspace_root, agent, hash, created_at) VALUES (?, ?, ?, ?)`,
			ws, agent, emissionHash(text), now); err != nil {
			return err
		}
		// ponytail: string-compared DATETIME prune; exact cutoff doesn't matter, reads re-check age in Go.
		_, err := tx.ExecContext(ctx, `DELETE FROM gate_emissions WHERE created_at < ?`, now.Add(-emissionLifetime))
		return err
	})
}

// keepEnds fits s into n bytes by keeping its head and tail around a
// marker, never splitting a rune.
func keepEnds(s string, n int) string {
	const marker = "\n…\n"
	if len(s) <= n {
		return s
	}
	half := (n - len(marker)) / 2
	head, tail := half, len(s)-half
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + marker + s[tail:]
}
