// Package store persists Breakroom state in an embedded SQLite database.
// SQLite is the source of truth; in-memory channels only wake waiters.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// Limits from docs/tool-contracts.md.
const (
	MaxContentBytes = 65536
	MaxDiffBytes    = 16384
)

const schemaVersion = 7

// ErrNoAgent is returned when an agent id does not exist in a workspace.
var ErrNoAgent = errors.New("no such agent")

var schemaV1 = []string{
	`CREATE TABLE agents (
		workspace_root TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		worktree_path TEXT NOT NULL,
		provider_hint TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'idle',
		plan_markdown TEXT NOT NULL DEFAULT '',
		current_step TEXT NOT NULL DEFAULT '',
		modified_files_json TEXT NOT NULL DEFAULT '[]',
		pid INTEGER,
		last_heartbeat DATETIME NOT NULL,
		PRIMARY KEY (workspace_root, agent_id)
	)`,
	`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_root TEXT NOT NULL,
		from_agent TEXT NOT NULL,
		to_agent TEXT NOT NULL,
		recipient TEXT NOT NULL,
		msg_type TEXT NOT NULL,
		content TEXT NOT NULL,
		diff_attachment TEXT NOT NULL DEFAULT '',
		read_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX idx_messages_unread ON messages (workspace_root, recipient, read_at)`,
	`CREATE INDEX idx_messages_from ON messages (workspace_root, from_agent, created_at)`,
	`CREATE INDEX idx_agents_workspace ON agents (workspace_root, status)`,
}

var schemaV2 = []string{
	`CREATE TABLE blueprints (
		workspace_root TEXT NOT NULL,
		name TEXT NOT NULL,
		architect TEXT NOT NULL,
		builder TEXT NOT NULL,
		goal TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'drafting',
		current_phase INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (workspace_root, name)
	)`,
	`CREATE TABLE phases (
		workspace_root TEXT NOT NULL,
		blueprint_name TEXT NOT NULL,
		phase INTEGER NOT NULL,
		title TEXT NOT NULL,
		objective TEXT NOT NULL,
		requirements_json TEXT NOT NULL,
		acceptance_json TEXT NOT NULL,
		constraints TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		rounds INTEGER NOT NULL DEFAULT 0,
		deliverable_json TEXT NOT NULL DEFAULT '',
		inspections_json TEXT NOT NULL DEFAULT '[]',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (workspace_root, blueprint_name, phase)
	)`,
	`CREATE INDEX idx_phases_lookup ON phases (workspace_root, blueprint_name, status)`,
}

// schemaV3 adds roadmaps, recorded human prompts and gate emissions.
var schemaV3 = []string{
	`ALTER TABLE blueprints ADD COLUMN require_roadmap INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE blueprint_roadmaps (
		workspace_root TEXT NOT NULL,
		name TEXT NOT NULL,
		version INTEGER NOT NULL,
		phases_json TEXT NOT NULL,
		out_of_scope TEXT NOT NULL DEFAULT '',
		proposed_at DATETIME NOT NULL,
		approved_at DATETIME,
		approved_via TEXT NOT NULL DEFAULT '',
		approval_quote TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (workspace_root, name, version)
	)`,
	`CREATE TABLE human_prompts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		workspace_root TEXT NOT NULL,
		agent TEXT NOT NULL,
		text TEXT NOT NULL,
		created_at DATETIME NOT NULL
	)`,
	`CREATE INDEX idx_prompts_agent ON human_prompts (workspace_root, agent, id)`,
	`CREATE TABLE gate_emissions (
		workspace_root TEXT NOT NULL,
		agent TEXT NOT NULL,
		hash TEXT NOT NULL,
		created_at DATETIME NOT NULL
	)`,
	`CREATE INDEX idx_emissions_agent ON gate_emissions (workspace_root, agent, hash)`,
}

// schemaV4 records the host process that spawned each agent's proxy, so
// hooks (children of the same host) can find their agent by ancestry.
var schemaV4 = []string{
	`ALTER TABLE agents ADD COLUMN host_pid INTEGER`,
}

// schemaV5 adds series (blueprints created without a builder, joined later
// by name) and the builders waiting for one to open.
var schemaV5 = []string{
	`ALTER TABLE blueprints ADD COLUMN series INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE series_waiters (
		workspace_root TEXT NOT NULL,
		agent TEXT NOT NULL,
		PRIMARY KEY (workspace_root, agent)
	)`,
}

// schemaV6 records each series' worktree, branch and base commit, and the
// base a roadmap asks a series to branch from.
var schemaV6 = []string{
	`ALTER TABLE blueprints ADD COLUMN worktree_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE blueprints ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE blueprints ADD COLUMN base_ref TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE blueprint_roadmaps ADD COLUMN base TEXT NOT NULL DEFAULT ''`,
}

// schemaV7 records the human's acceptance of finished work and how it
// landed.
var schemaV7 = []string{
	`ALTER TABLE blueprints ADD COLUMN accept_requested_at DATETIME`,
	`ALTER TABLE blueprints ADD COLUMN accepted_at DATETIME`,
	`ALTER TABLE blueprints ADD COLUMN verdict_via TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE blueprints ADD COLUMN verdict_quote TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE blueprints ADD COLUMN review_rounds INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE blueprints ADD COLUMN landing_method TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE blueprints ADD COLUMN landing_ref TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE blueprints ADD COLUMN landed_at DATETIME`,
}

// Store is a SQLite-backed Breakroom state database.
type Store struct {
	db *sql.DB
}

// Open opens (creating parent directories) the database at path, applies
// pragmas, and runs pending migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Single connection: this workload is tiny, and one writer removes an
	// entire class of SQLITE_BUSY flakiness.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate brings the database schema up to date, one version per transaction.
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion {
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d)", v, schemaVersion)
	}
	for v < schemaVersion {
		v++
		// SQLite DDL and user_version are transactional: a step that fails
		// partway leaves nothing behind, so the next start retries it.
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err := applyMigration(tx, v); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`PRAGMA user_version=` + strconv.Itoa(v)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs the statements for one schema version.
func applyMigration(db interface {
	Exec(query string, args ...any) (sql.Result, error)
}, v int) error {
	schemas := map[int][]string{1: schemaV1, 2: schemaV2, 3: schemaV3, 4: schemaV4, 5: schemaV5, 6: schemaV6, 7: schemaV7}
	stmts, ok := schemas[v]
	if !ok {
		return fmt.Errorf("store: unknown migration v%d", v)
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// ModifiedFile is one entry of an agent's git status cache.
type ModifiedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // two-letter porcelain XY code
}

// Agent is a registered coding agent.
type Agent struct {
	WorkspaceRoot string
	AgentID       string
	WorktreePath  string
	ProviderHint  string
	Role          string
	Status        string
	PlanMarkdown  string
	CurrentStep   string
	ModifiedFiles []ModifiedFile
	PID           *int64
	LastHeartbeat time.Time
	HostPID       *int64 // the process that spawned the proxy (the agent's host)
}

// Message is one persisted message row. To keeps the original address
// ('*' for broadcasts); Recipient is always the concrete agent id.
type Message struct {
	ID             int64
	WorkspaceRoot  string
	From           string
	To             string
	Recipient      string
	Type           string
	Content        string
	DiffAttachment string
	ReadAt         *time.Time
	CreatedAt      time.Time
}

// nullInt64 converts an optional int to its SQL form.
func nullInt64(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// collect scans every row with scan and closes rows.
func collect[T any](rows *sql.Rows, scan func(interface{ Scan(...any) error }) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanAgent reads one agents row, decoding its file list.
func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var filesJSON string
	var pid, hostPID sql.NullInt64
	err := row.Scan(&a.WorkspaceRoot, &a.AgentID, &a.WorktreePath, &a.ProviderHint,
		&a.Role, &a.Status, &a.PlanMarkdown, &a.CurrentStep, &filesJSON, &pid, &a.LastHeartbeat, &hostPID)
	if err != nil {
		return nil, err
	}
	if pid.Valid {
		a.PID = &pid.Int64
	}
	if hostPID.Valid {
		a.HostPID = &hostPID.Int64
	}
	a.ModifiedFiles = []ModifiedFile{}
	if err := json.Unmarshal([]byte(filesJSON), &a.ModifiedFiles); err != nil {
		return nil, fmt.Errorf("store: bad modified_files_json: %w", err)
	}
	return &a, nil
}

// scanMessage reads one messages row.
func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var readAt sql.NullTime
	err := row.Scan(&m.ID, &m.WorkspaceRoot, &m.From, &m.To, &m.Recipient,
		&m.Type, &m.Content, &m.DiffAttachment, &readAt, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	if readAt.Valid {
		m.ReadAt = &readAt.Time
	}
	return &m, nil
}

// RegisterParams describes an agent asking for an identity.
type RegisterParams struct {
	WorkspaceRoot string
	WorktreePath  string
	Role          string
	PlanMarkdown  string
	ProviderHint  string
	PID           *int64
	HostPID       *int64 // the proxy's parent process
	Name          string // optional: resume this offline identity
	Series        string // optional: resume this series' offline participant
}

// ErrNameInUse is returned when resuming an identity that is still live.
var ErrNameInUse = errors.New("agent name in use")

// nameWords are short, unambiguous, easy-to-type stems for agent names.
var nameWords = []string{
	"badger", "bear", "bison", "camel", "cobra", "crane", "crow", "deer",
	"dingo", "dove", "duck", "eagle", "egret", "falcon", "ferret", "frog",
	"gecko", "goat", "goose", "hawk", "heron", "hippo", "horse", "hyena",
	"ibis", "koala", "lemur", "lion", "llama", "lynx", "macaw", "mink",
	"mole", "moose", "moth", "mule", "newt", "orca", "otter", "owl",
	"panda", "puma", "quail", "raven", "rhino", "robin", "seal", "shark",
	"skunk", "sloth", "snail", "squid", "stork", "swan", "tapir", "tiger",
	"toad", "trout", "viper", "walrus", "whale", "wolf", "wombat", "yak",
}

// genName proposes a candidate name like "otter-417"; attempt counts
// collisions so far (tests stub this for deterministic ids).
var genName = func(attempt int) string {
	return nameWords[rand.IntN(len(nameWords))] + "-" + strconv.Itoa(100+rand.IntN(900))
}

// RegisterAgent gives an agent its identity. Names are never reused; an
// agent gets its old name, inbox, and roles back by name, or by returning
// from the same worktree with the same role.
func (s *Store) RegisterAgent(ctx context.Context, p RegisterParams) (*Agent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()

	id := p.Name
	if id != "" {
		var status string
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM agents WHERE workspace_root=? AND agent_id=?`,
			p.WorkspaceRoot, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNoAgent, id)
		}
		if err != nil {
			return nil, err
		}
		if status != "offline" {
			return nil, fmt.Errorf("%w: %s", ErrNameInUse, id)
		}
	} else {
		// Agents of a series all start in the main checkout, so the series
		// names an identity better than the folder does.
		if p.Series != "" {
			err := tx.QueryRowContext(ctx,
				`SELECT a.agent_id FROM agents a JOIN blueprints b
				   ON b.workspace_root=a.workspace_root AND (b.architect=a.agent_id OR b.builder=a.agent_id)
				 WHERE a.workspace_root=? AND b.name=? AND a.role=? AND a.status='offline'
				 ORDER BY a.rowid LIMIT 1`,
				p.WorkspaceRoot, p.Series, p.Role).Scan(&id)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		if id == "" {
			err := tx.QueryRowContext(ctx,
				`SELECT agent_id FROM agents
				 WHERE workspace_root=? AND worktree_path=? AND role=? AND status='offline'
				 ORDER BY rowid LIMIT 1`,
				p.WorkspaceRoot, p.WorktreePath, p.Role).Scan(&id)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
	}
	if id != "" {
		_, err = tx.ExecContext(ctx,
			`UPDATE agents SET worktree_path=?, provider_hint=?, role=?, status='idle',
				plan_markdown=?, current_step='', modified_files_json='[]', pid=?, last_heartbeat=?, host_pid=?
			 WHERE workspace_root=? AND agent_id=?`,
			p.WorktreePath, p.ProviderHint, p.Role, p.PlanMarkdown, nullInt64(p.PID),
			now, nullInt64(p.HostPID), p.WorkspaceRoot, id)
	} else {
		if id, err = freshName(ctx, tx, p.WorkspaceRoot); err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO agents (workspace_root, agent_id, worktree_path, provider_hint, role,
				status, plan_markdown, current_step, modified_files_json, pid, last_heartbeat, host_pid)
			 VALUES (?, ?, ?, ?, ?, 'idle', ?, '', '[]', ?, ?, ?)`,
			p.WorkspaceRoot, id, p.WorktreePath, p.ProviderHint, p.Role,
			p.PlanMarkdown, nullInt64(p.PID), now, nullInt64(p.HostPID))
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAgent(ctx, p.WorkspaceRoot, id)
}

// freshName picks a name never used in the workspace (offline rows count).
func freshName(ctx context.Context, tx *sql.Tx, ws string) (string, error) {
	for attempt := range 1000 {
		id := genName(attempt)
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM agents WHERE workspace_root=? AND agent_id=?`, ws, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("store: no free agent name")
}

// ReleaseAgent marks an agent offline and clears its PID. Its unread inbox
// is kept so a reconnecting agent still sees messages sent while away.
// Any pending series-waiter registration is removed.
func (s *Store) ReleaseAgent(ctx context.Context, workspaceRoot, agentID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET status='offline', pid=NULL, last_heartbeat=?
		 WHERE workspace_root=? AND agent_id=?`,
		time.Now().UTC(), workspaceRoot, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNoAgent, agentID)
	}
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM series_waiters WHERE workspace_root=? AND agent=?`,
		workspaceRoot, agentID)
	return nil
}

// AgentUpdate carries optional agent fields; nil means keep.
type AgentUpdate struct {
	Status        *string
	CurrentStep   *string
	PlanMarkdown  *string
	ModifiedFiles []ModifiedFile // nil means keep
}

var validStatuses = map[string]bool{
	"idle": true, "active": true, "waiting": true, "completed": true, "offline": true,
}

// UpdateAgent applies an update and refreshes the heartbeat.
func (s *Store) UpdateAgent(ctx context.Context, workspaceRoot, agentID string, u AgentUpdate) (*Agent, error) {
	if u.Status != nil && !validStatuses[*u.Status] {
		return nil, fmt.Errorf("store: bad status %q", *u.Status)
	}
	sets := []string{"last_heartbeat=?"}
	args := []any{time.Now().UTC()}
	if u.Status != nil {
		sets, args = append(sets, "status=?"), append(args, *u.Status)
	}
	if u.CurrentStep != nil {
		sets, args = append(sets, "current_step=?"), append(args, *u.CurrentStep)
	}
	if u.PlanMarkdown != nil {
		sets, args = append(sets, "plan_markdown=?"), append(args, *u.PlanMarkdown)
	}
	if u.ModifiedFiles != nil {
		files, err := json.Marshal(u.ModifiedFiles)
		if err != nil {
			return nil, err
		}
		sets, args = append(sets, "modified_files_json=?"), append(args, string(files))
	}
	args = append(args, workspaceRoot, agentID)
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET `+strings.Join(sets, ",")+` WHERE workspace_root=? AND agent_id=?`, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoAgent, agentID)
	}
	return s.GetAgent(ctx, workspaceRoot, agentID)
}

// GetAgent returns one agent.
func (s *Store) GetAgent(ctx context.Context, workspaceRoot, agentID string) (*Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx,
		`SELECT workspace_root, agent_id, worktree_path, provider_hint, role, status,
			plan_markdown, current_step, modified_files_json, pid, last_heartbeat, host_pid
		 FROM agents WHERE workspace_root=? AND agent_id=?`, workspaceRoot, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoAgent, agentID)
	}
	return a, err
}

// ListAgents returns agents in a workspace in registration order.
func (s *Store) ListAgents(ctx context.Context, workspaceRoot string, includeOffline bool) ([]*Agent, error) {
	q := `SELECT workspace_root, agent_id, worktree_path, provider_hint, role, status,
		plan_markdown, current_step, modified_files_json, pid, last_heartbeat, host_pid
		FROM agents WHERE workspace_root=?`
	if !includeOffline {
		q += ` AND status<>'offline'`
	}
	q += ` ORDER BY rowid`
	rows, err := s.db.QueryContext(ctx, q, workspaceRoot)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAgent)
}

// CountPresent returns the number of agents that are not offline/completed
// across all workspaces (gates daemon idle shutdown).
func (s *Store) CountPresent(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE status NOT IN ('offline','completed')`).Scan(&n)
	return n, err
}

// CountCompleted returns the number of completed agents still connected
// (not released) across all workspaces; they don't count as present.
func (s *Store) CountCompleted(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE status='completed'`).Scan(&n)
	return n, err
}

// ListOnlineAgents returns every agent not yet released, completed ones
// included, across all workspaces (feeds the dead-proxy reaper: a completed
// agent killed without a release still counts as live for hooks).
func (s *Store) ListOnlineAgents(ctx context.Context) ([]*Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT workspace_root, agent_id, worktree_path, provider_hint, role, status,
			plan_markdown, current_step, modified_files_json, pid, last_heartbeat, host_pid
		 FROM agents WHERE status<>'offline' ORDER BY workspace_root, agent_id`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAgent)
}

// SendParams describes a message to persist.
type SendParams struct {
	WorkspaceRoot  string
	From           string
	To             string // agent id or '*'
	Type           string // '' defaults to 'chat'
	Content        string
	DiffAttachment string
	// Recipients, when non-nil, limits a broadcast to these agents (the
	// sender's visible peers).
	Recipients []string
}

// SentMessage is one persisted row from SendMessage.
type SentMessage struct {
	ID        int64
	Recipient string
}

var validMsgTypes = map[string]bool{
	"chat": true, "handoff": true, "conflict_alert": true, "review_req": true,
	"blueprint_spec": true, "blueprint_punchlist": true, "blueprint_signoff": true,
	"blueprint_roadmap": true, "blueprint_series": true,
}

// SendMessage persists a message, fanning broadcasts out to one row per
// non-offline recipient (excluding the sender).
func (s *Store) SendMessage(ctx context.Context, p SendParams) ([]SentMessage, error) {
	if p.Content == "" {
		return nil, errors.New("store: content is empty")
	}
	if len(p.Content) > MaxContentBytes {
		return nil, fmt.Errorf("store: content exceeds %d bytes", MaxContentBytes)
	}
	if len(p.DiffAttachment) > MaxDiffBytes {
		return nil, fmt.Errorf("store: diff attachment exceeds %d bytes", MaxDiffBytes)
	}
	msgType := p.Type
	if msgType == "" {
		msgType = "chat"
	}
	if !validMsgTypes[msgType] {
		return nil, fmt.Errorf("store: bad msg_type %q", p.Type)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var recipients []string
	if p.To == "*" {
		rows, err := tx.QueryContext(ctx,
			`SELECT agent_id FROM agents
			 WHERE workspace_root=? AND status<>'offline' AND agent_id<>?`,
			p.WorkspaceRoot, p.From)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return nil, err
			}
			recipients = append(recipients, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if p.Recipients != nil {
			allowed := map[string]bool{}
			for _, r := range p.Recipients {
				allowed[r] = true
			}
			kept := recipients[:0]
			for _, r := range recipients {
				if allowed[r] {
					kept = append(kept, r)
				}
			}
			recipients = kept
		}
	} else {
		var exists bool
		err := tx.QueryRowContext(ctx,
			`SELECT true FROM agents WHERE workspace_root=? AND agent_id=?`,
			p.WorkspaceRoot, p.To).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNoAgent, p.To)
		}
		if err != nil {
			return nil, err
		}
		recipients = []string{p.To}
	}
	var sent []SentMessage
	for _, r := range recipients {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO messages (workspace_root, from_agent, to_agent, recipient,
				msg_type, content, diff_attachment)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.WorkspaceRoot, p.From, p.To, r, msgType, p.Content, p.DiffAttachment)
		if err != nil {
			return nil, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		sent = append(sent, SentMessage{ID: id, Recipient: r})
	}
	return sent, tx.Commit()
}

// PeekUnread returns an agent's unread messages without marking them read.
func (s *Store) PeekUnread(ctx context.Context, workspaceRoot, recipient string) ([]*Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_root, from_agent, to_agent, recipient, msg_type,
			content, diff_attachment, read_at, created_at
		 FROM messages
		 WHERE workspace_root=? AND recipient=? AND read_at IS NULL ORDER BY id`,
		workspaceRoot, recipient)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMessage)
}

// MarkRead marks the given message ids read. Only the listed ids are
// touched, so mail arriving between a peek and its mark stays unread.
func (s *Store) MarkRead(ctx context.Context, workspaceRoot, recipient string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{time.Now().UTC(), workspaceRoot, recipient}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET read_at=? WHERE workspace_root=? AND recipient=?
		 AND read_at IS NULL AND id IN (`+strings.Repeat("?,", len(ids)-1)+`?)`, args...)
	return err
}

// TakeUnread returns an agent's unread messages and marks exactly those read.
func (s *Store) TakeUnread(ctx context.Context, workspaceRoot, recipient string) ([]*Message, error) {
	msgs, err := s.PeekUnread(ctx, workspaceRoot, recipient)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return msgs, s.MarkRead(ctx, workspaceRoot, recipient, ids)
}

// ListThread returns a workspace's messages newest first, counting each
// broadcast once rather than once per recipient.
func (s *Store) ListThread(ctx context.Context, workspaceRoot string, limit int) ([]*Message, error) {
	// ponytail: fan-out rows are grouped by sender, content, and second, so two
	// identical broadcasts in the same second merge; add a batch column if that
	// ever matters.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_root, from_agent, to_agent, recipient, msg_type,
			content, diff_attachment, read_at, created_at
		 FROM messages WHERE workspace_root=? AND id IN (
			SELECT MIN(id) FROM messages WHERE workspace_root=?
			GROUP BY CASE WHEN to_agent='*'
				THEN from_agent || char(0) || content || char(0) || created_at
				ELSE CAST(id AS TEXT) END)
		 ORDER BY id DESC LIMIT ?`,
		workspaceRoot, workspaceRoot, limit)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanMessage)
}

// Prune deletes read mail and closed blueprints older than cutoff.
// Unread mail and open blueprints are never touched.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (msgs, blueprints int64, err error) {
	// ponytail: string-compared DATETIMEs, all written by Go in UTC; day-scale
	// cutoffs don't care about sub-second ordering.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	cutoff = cutoff.UTC()
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE read_at IS NOT NULL AND read_at < ?`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	msgs, _ = res.RowsAffected()
	old := `SELECT workspace_root, name FROM blueprints WHERE status='handover' AND updated_at < ?`
	for _, q := range []string{
		`DELETE FROM phases WHERE (workspace_root, blueprint_name) IN (` + old + `)`,
		`DELETE FROM blueprint_roadmaps WHERE (workspace_root, name) IN (` + old + `)`,
	} {
		if _, err := tx.ExecContext(ctx, q, cutoff); err != nil {
			return 0, 0, err
		}
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM blueprints WHERE status='handover' AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	blueprints, _ = res.RowsAffected()
	return msgs, blueprints, tx.Commit()
}
