package store

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// openTest opens a store on a fresh temp database.
func openTest(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// Deterministic ids: the first free agent-N (freshName skips taken ones).
	orig := genName
	genName = func(attempt int) string { return "agent-" + strconv.Itoa(attempt+1) }
	t.Cleanup(func() { genName = orig })
	return s, context.Background()
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// mustRegister registers an agent, failing the test on error.
func mustRegister(t *testing.T, s *Store, ctx context.Context, ws, wt, role string) *Agent {
	t.Helper()
	a, err := s.RegisterAgent(ctx, RegisterParams{
		WorkspaceRoot: ws, WorktreePath: wt, Role: role, PlanMarkdown: "# plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPragmasAndMigration(t *testing.T) {
	s, _ := openTest(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	// Reopen migrates nothing and keeps working.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterAllocate(t *testing.T) {
	s, ctx := openTest(t)
	a1 := mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	a2 := mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	if a1.AgentID != "agent-1" || a2.AgentID != "agent-2" {
		t.Fatalf("ids = %s, %s; want agent-1, agent-2", a1.AgentID, a2.AgentID)
	}
	if a1.Status != "idle" || a1.PlanMarkdown != "# plan" {
		t.Fatalf("unexpected agent: %+v", a1)
	}
	if a1.PID != nil {
		t.Fatalf("PID = %v, want nil", *a1.PID)
	}
	// PID round-trips.
	a3, err := s.RegisterAgent(ctx, RegisterParams{
		WorkspaceRoot: "/ws", WorktreePath: "/ws/wt-c", Role: "docs", PID: ptr(int64(4242)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if a3.PID == nil || *a3.PID != 4242 {
		t.Fatalf("PID = %v, want 4242", a3.PID)
	}
}

func TestRegisterReconnectKeepsInbox(t *testing.T) {
	s, ctx := openTest(t)
	a1 := mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	_ = s.AddSeriesWaiter(ctx, "/ws", a1.AgentID)
	if err := s.ReleaseAgent(ctx, "/ws", a1.AgentID); err != nil {
		t.Fatal(err)
	}
	waiters, _ := s.ListSeriesWaiters(ctx, "/ws")
	if len(waiters) != 0 {
		t.Fatalf("waiters after release = %v, want empty", waiters)
	}
	if _, err := s.SendMessage(ctx, SendParams{
		WorkspaceRoot: "/ws", From: "agent-2", To: "agent-1", Content: "while you were out",
	}); err != nil {
		t.Fatal(err)
	}
	back, err := s.RegisterAgent(ctx, RegisterParams{
		WorkspaceRoot: "/ws", WorktreePath: "/ws/wt-a", Role: "backend",
	})
	if err != nil {
		t.Fatal(err)
	}
	if back.AgentID != "agent-1" || back.Status != "idle" {
		t.Fatalf("reconnect = %+v, want id agent-1 status idle", back)
	}
	unread, err := s.TakeUnread(ctx, "/ws", "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 || unread[0].Content != "while you were out" {
		t.Fatalf("unread = %+v, want the queued message", unread)
	}
}

// Names are never reused: a newcomer cannot inherit an offline agent's
// inbox (or blueprint roles); the owner resumes it by name.
func TestRegisterNeverReusesNames(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "architect")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	if _, err := s.SendMessage(ctx, SendParams{
		WorkspaceRoot: "/ws", From: "agent-2", To: "agent-1", Content: "for agent-1 only",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAgent(ctx, "/ws", "agent-1"); err != nil {
		t.Fatal(err)
	}
	stranger := mustRegister(t, s, ctx, "/ws", "/ws/wt-c", "docs")
	if stranger.AgentID != "agent-3" {
		t.Fatalf("newcomer id = %s, want fresh agent-3", stranger.AgentID)
	}
	if msgs, _ := s.PeekUnread(ctx, "/ws", "agent-1"); len(msgs) != 1 {
		t.Fatalf("offline inbox = %d, want 1 kept", len(msgs))
	}
	// Explicit resume works across a role change and keeps the inbox.
	back, err := s.RegisterAgent(ctx, RegisterParams{
		WorkspaceRoot: "/ws", WorktreePath: "/ws/wt-a", Role: "lead", Name: "agent-1",
	})
	if err != nil || back.AgentID != "agent-1" || back.Role != "lead" || back.Status != "idle" {
		t.Fatalf("resume = %+v, %v", back, err)
	}
	if msgs, _ := s.TakeUnread(ctx, "/ws", "agent-1"); len(msgs) != 1 {
		t.Fatalf("resumed inbox = %d, want 1", len(msgs))
	}
	// Live names and unknown names cannot be claimed.
	for _, tc := range []struct {
		name    string
		agent   string
		wantErr error
	}{
		{"live name", "agent-2", ErrNameInUse},
		{"unknown name", "agent-9", ErrNoAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.RegisterAgent(ctx, RegisterParams{
				WorkspaceRoot: "/ws", WorktreePath: "/ws", Role: "x", Name: tc.agent,
			}); !errors.Is(err, tc.wantErr) {
				t.Fatalf("resume %s = %v, want %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestGenName(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+-[1-9][0-9]{2}$`)
	seen := map[string]bool{}
	for i := range 200 {
		n := genName(i)
		if !re.MatchString(n) {
			t.Fatalf("name %q does not look like word-NNN", n)
		}
		seen[n] = true
	}
	if len(seen) < 150 {
		t.Fatalf("only %d distinct names in 200 draws", len(seen))
	}
}

func TestSendDirect(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	ids, err := s.SendMessage(ctx, SendParams{
		WorkspaceRoot: "/ws", From: "agent-1", To: "agent-2",
		Type: "review_req", Content: "please review", DiffAttachment: "diff --git",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want 1 row", ids)
	}
	msgs, err := s.TakeUnread(ctx, "/ws", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("unread = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.From != "agent-1" || m.To != "agent-2" || m.Recipient != "agent-2" ||
		m.Type != "review_req" || m.Content != "please review" || m.DiffAttachment != "diff --git" {
		t.Fatalf("unexpected message: %+v", m)
	}
	if time.Since(m.CreatedAt) > time.Minute {
		t.Fatalf("CreatedAt = %v, want recent", m.CreatedAt)
	}

	cases := []struct {
		name string
		send SendParams
	}{
		{"unknown", SendParams{WorkspaceRoot: "/ws", From: "agent-1", To: "agent-9", Content: "x"}},
		{"empty", SendParams{WorkspaceRoot: "/ws", From: "agent-1", To: "agent-2", Content: ""}},
		{"bad type", SendParams{WorkspaceRoot: "/ws", From: "agent-1", To: "agent-2", Content: "x", Type: "yell"}},
		{"too long", SendParams{WorkspaceRoot: "/ws", From: "agent-1", To: "agent-2", Content: strings.Repeat("x", MaxContentBytes+1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.SendMessage(ctx, c.send); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
	if _, err := s.SendMessage(ctx, SendParams{
		WorkspaceRoot: "/ws", From: "agent-1", To: "agent-9", Content: "x",
	}); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("unknown agent err = %v, want ErrNoAgent", err)
	}
}

func TestBroadcastFanout(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-c", "docs")
	if _, err := s.SendMessage(ctx, SendParams{
		WorkspaceRoot: "/ws", From: "agent-1", To: "*", Content: "hello all",
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"agent-2", "agent-3"} {
		msgs, err := s.TakeUnread(ctx, "/ws", id)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 || msgs[0].To != "*" || msgs[0].Recipient != id {
			t.Fatalf("%s got %+v, want 1 broadcast row", id, msgs)
		}
	}
	if msgs, _ := s.TakeUnread(ctx, "/ws", "agent-1"); len(msgs) != 0 {
		t.Fatalf("sender got %d echo rows, want 0", len(msgs))
	}
	// Offline agents are excluded from new broadcasts.
	if err := s.ReleaseAgent(ctx, "/ws", "agent-3"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, SendParams{
		WorkspaceRoot: "/ws", From: "agent-1", To: "*", Content: "again",
	}); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := s.TakeUnread(ctx, "/ws", "agent-2"); len(msgs) != 1 {
		t.Fatalf("agent-2 got %d, want 1", len(msgs))
	}
	if msgs, _ := s.TakeUnread(ctx, "/ws", "agent-3"); len(msgs) != 0 {
		t.Fatalf("offline agent-3 got %d, want 0", len(msgs))
	}
}

func TestTakeUnreadMarksReadKeepsHistory(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	for _, c := range []string{"one", "two"} {
		if _, err := s.SendMessage(ctx, SendParams{
			WorkspaceRoot: "/ws", From: "agent-1", To: "agent-2", Content: c,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.TakeUnread(ctx, "/ws", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Content != "one" || first[1].Content != "two" {
		t.Fatalf("first take = %+v, want [one two]", first)
	}
	second, err := s.TakeUnread(ctx, "/ws", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second take = %d, want 0", len(second))
	}
	history, err := s.ListThread(ctx, "/ws", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Content != "two" {
		t.Fatalf("history = %+v, want newest-first [two one]", history)
	}
}

func TestListAgents(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	if err := s.ReleaseAgent(ctx, "/ws", "agent-2"); err != nil {
		t.Fatal(err)
	}
	online, err := s.ListAgents(ctx, "/ws", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(online) != 1 || online[0].AgentID != "agent-1" {
		t.Fatalf("online = %+v, want [agent-1]", online)
	}
	all, err := s.ListAgents(ctx, "/ws", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].AgentID != "agent-1" || all[1].AgentID != "agent-2" {
		t.Fatalf("all = %+v, want slot order", all)
	}
	if err := s.ReleaseAgent(ctx, "/ws", "agent-9"); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("release unknown err = %v, want ErrNoAgent", err)
	}
}

func TestUpdateAgent(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	a, err := s.UpdateAgent(ctx, "/ws", "agent-1", AgentUpdate{
		Status:        ptr("active"),
		CurrentStep:   ptr("building api"),
		PlanMarkdown:  ptr("# new plan"),
		ModifiedFiles: []ModifiedFile{{Path: "a.go", Status: " M"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "active" || a.CurrentStep != "building api" || a.PlanMarkdown != "# new plan" {
		t.Fatalf("updated = %+v", a)
	}
	if len(a.ModifiedFiles) != 1 || a.ModifiedFiles[0].Path != "a.go" {
		t.Fatalf("files = %+v", a.ModifiedFiles)
	}
	if time.Since(a.LastHeartbeat) > time.Minute {
		t.Fatalf("heartbeat = %v, want recent", a.LastHeartbeat)
	}
	// Nil fields are kept.
	a2, err := s.UpdateAgent(ctx, "/ws", "agent-1", AgentUpdate{Status: ptr("waiting")})
	if err != nil {
		t.Fatal(err)
	}
	if a2.Status != "waiting" || a2.CurrentStep != "building api" || len(a2.ModifiedFiles) != 1 {
		t.Fatalf("partial update = %+v", a2)
	}
	for _, tc := range []struct {
		name    string
		agent   string
		update  AgentUpdate
		wantErr error
	}{
		{"bad status", "agent-1", AgentUpdate{Status: ptr("napping")}, nil},
		{"unknown agent", "agent-9", AgentUpdate{}, ErrNoAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.UpdateAgent(ctx, "/ws", tc.agent, tc.update)
			if tc.wantErr == nil {
				if err == nil {
					t.Fatal("bad status: want error, got nil")
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("unknown agent err = %v, want ErrNoAgent", err)
			}
		})
	}
}

func TestCountPresent(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	if n, _ := s.CountPresent(ctx); n != 2 {
		t.Fatalf("present = %d, want 2", n)
	}
	if err := s.ReleaseAgent(ctx, "/ws", "agent-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAgent(ctx, "/ws", "agent-2", AgentUpdate{Status: ptr("completed")}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountPresent(ctx); n != 0 {
		t.Fatalf("present = %d, want 0", n)
	}
	if n, _ := s.CountCompleted(ctx); n != 1 {
		t.Fatalf("completed = %d, want 1 (released agent excluded)", n)
	}
}

func TestWorkspaceIsolation(t *testing.T) {
	s, ctx := openTest(t)
	a1 := mustRegister(t, s, ctx, "/ws1", "/ws1/wt", "backend")
	a2 := mustRegister(t, s, ctx, "/ws2", "/ws2/wt", "backend")
	if a1.AgentID != "agent-1" || a2.AgentID != "agent-1" {
		t.Fatalf("ids = %s, %s; want agent-1 in each workspace", a1.AgentID, a2.AgentID)
	}
	mustRegister(t, s, ctx, "/ws1", "/ws1/wt-b", "frontend")
	if _, err := s.SendMessage(ctx, SendParams{
		WorkspaceRoot: "/ws1", From: "agent-2", To: "agent-1", Content: "ws1 only",
	}); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := s.TakeUnread(ctx, "/ws2", "agent-1"); len(msgs) != 0 {
		t.Fatalf("ws2 sees %d ws1 messages, want 0", len(msgs))
	}
	if msgs, _ := s.TakeUnread(ctx, "/ws1", "agent-1"); len(msgs) != 1 {
		t.Fatalf("ws1 sees %d, want 1", len(msgs))
	}
}

func TestPeekAndMarkRead(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/wt-a", "backend")
	mustRegister(t, s, ctx, "/ws", "/ws/wt-b", "frontend")
	for _, c := range []string{"one", "two"} {
		if _, err := s.SendMessage(ctx, SendParams{
			WorkspaceRoot: "/ws", From: "agent-1", To: "agent-2", Content: c,
		}); err != nil {
			t.Fatal(err)
		}
	}
	peek, err := s.PeekUnread(ctx, "/ws", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(peek) != 2 {
		t.Fatalf("peek = %d, want 2", len(peek))
	}
	// Peek marks nothing.
	again, err := s.PeekUnread(ctx, "/ws", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Fatalf("second peek = %d, want 2", len(again))
	}
	// Marking one leaves the other.
	if err := s.MarkRead(ctx, "/ws", "agent-2", []int64{peek[0].ID}); err != nil {
		t.Fatal(err)
	}
	rest, err := s.PeekUnread(ctx, "/ws", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Content != "two" {
		t.Fatalf("rest = %+v, want [two]", rest)
	}
	if err := s.MarkRead(ctx, "/ws", "agent-2", nil); err != nil {
		t.Fatalf("mark empty = %v, want nil", err)
	}
}

func TestListOnlineAgents(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws1", "/ws1/wt", "backend")
	mustRegister(t, s, ctx, "/ws2", "/ws2/wt", "backend")
	mustRegister(t, s, ctx, "/ws2", "/ws2/wt-b", "frontend")
	if err := s.ReleaseAgent(ctx, "/ws2", "agent-2"); err != nil {
		t.Fatal(err)
	}
	present, err := s.ListOnlineAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(present) != 2 {
		t.Fatalf("present = %d, want 2 across workspaces", len(present))
	}
}

// A broadcast is stored once per recipient; the thread view (compaction)
// must count it once, and LIMIT must count logical messages.
func TestListThreadDedupesBroadcasts(t *testing.T) {
	s, ctx := openTest(t)
	for _, wt := range []string{"/ws/a", "/ws/b", "/ws/c", "/ws/d"} {
		mustRegister(t, s, ctx, "/ws", wt, "r"+wt)
	}
	send := func(to, content string) {
		t.Helper()
		if _, err := s.SendMessage(ctx, SendParams{WorkspaceRoot: "/ws", From: "agent-1", To: to, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	send("agent-2", "direct one")
	send("*", "to everyone") // 3 rows, one logical message
	send("agent-3", "direct two")
	msgs, err := s.ListThread(ctx, "/ws", 50)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		got = append(got, m.Content)
	}
	want := []string{"direct two", "to everyone", "direct one"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("thread = %q, want %q (newest first)", got, want)
	}
	if msgs, _ := s.ListThread(ctx, "/ws", 2); len(msgs) != 2 || msgs[1].Content != "to everyone" {
		t.Fatalf("limit 2 = %d messages, want 2 logical", len(msgs))
	}
}

func TestHostPIDPersists(t *testing.T) {
	s, ctx := openTest(t)
	a, err := s.RegisterAgent(ctx, RegisterParams{WorkspaceRoot: "/ws", WorktreePath: "/ws/a", Role: "r", HostPID: ptr(int64(4242))})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetAgent(ctx, "/ws", a.AgentID); got.HostPID == nil || *got.HostPID != 4242 {
		t.Fatalf("host pid = %v, want 4242", got.HostPID)
	}
	b := mustRegister(t, s, ctx, "/ws", "/ws/b", "r")
	if b.HostPID != nil {
		t.Fatalf("host pid = %v, want nil when not sent", *b.HostPID)
	}
}

func TestResumeBySeries(t *testing.T) {
	s, ctx := openTest(t)
	b := mustRegister(t, s, ctx, "/ws", "/ws", "builder")
	if err := s.InitBlueprint(ctx, "/ws", "s1", "arch", "", "g"); err != nil {
		t.Fatal(err)
	}
	if err := s.JoinBlueprint(ctx, "/ws", "s1", b.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAgent(ctx, "/ws", b.AgentID); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, ctx, "/ws", "/ws", "builder-other") // an unrelated offline-able peer
	got, err := s.RegisterAgent(ctx, RegisterParams{WorkspaceRoot: "/ws", WorktreePath: "/ws/elsewhere", Role: "builder", Series: "s1"})
	if err != nil || got.AgentID != b.AgentID {
		t.Fatalf("resume by series = %v (%v), want %s", got, err, b.AgentID)
	}
}

// Prune drops read mail and closed blueprints past the cutoff, never unread
// mail or open blueprints.
func TestPrune(t *testing.T) {
	s, ctx := openTest(t)
	mustRegister(t, s, ctx, "/ws", "/ws/a", "x")
	mustRegister(t, s, ctx, "/ws", "/ws/b", "y")
	send := func(c string) {
		if _, err := s.SendMessage(ctx, SendParams{WorkspaceRoot: "/ws", From: "agent-1", To: "agent-2", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	send("read")
	if _, err := s.TakeUnread(ctx, "/ws", "agent-2"); err != nil {
		t.Fatal(err)
	}
	send("unread")
	if n, _, err := s.Prune(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("prune before anything is old = %d, %v", n, err)
	}
	if n, _, err := s.Prune(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d messages (%v), want 1", n, err)
	}
	left, _ := s.PeekUnread(ctx, "/ws", "agent-2")
	if len(left) != 1 || left[0].Content != "unread" {
		t.Fatalf("unread after prune = %+v", left)
	}

	bs, ws := signedOff(t, "done")
	if err := bs.CloseBlueprint(ctx, ws, "done"); err != nil {
		t.Fatal(err)
	}
	if err := bs.InitBlueprint(ctx, ws, "open", "arch", "build", "g"); err != nil {
		t.Fatal(err)
	}
	if _, n, err := bs.Prune(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d blueprints (%v), want 1", n, err)
	}
	if _, err := bs.GetBlueprint(ctx, ws, "done"); !errors.Is(err, ErrNoBlueprint) {
		t.Fatalf("closed blueprint kept: %v", err)
	}
	if _, err := bs.GetBlueprint(ctx, ws, "open"); err != nil {
		t.Fatalf("open blueprint pruned: %v", err)
	}
	var phases int
	if err := bs.db.QueryRow(`SELECT COUNT(*) FROM phases WHERE blueprint_name='done'`).Scan(&phases); err != nil || phases != 0 {
		t.Fatalf("phases left = %d, %v", phases, err)
	}
}
