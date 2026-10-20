package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/search-3d/breakroom/internal/store"
)

// newTestServer returns a server on a fresh temp database.
func newTestServer(t *testing.T) *server {
	t.Helper()
	return newServer(openStore(t), defaultConfig())
}

// doPost sends body as JSON to path and returns the status and decoded reply.
func doPost(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: bad json %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, out
}

// wantErr fails the test unless the reply is an error with the given code.
func wantErr(t *testing.T, code int, out map[string]any, wantCode string) {
	t.Helper()
	errStr, _ := out["error"].(string)
	if code < 400 || !strings.HasPrefix(errStr, wantCode+":") {
		t.Fatalf("code=%d err=%q, want %s:*", code, errStr, wantCode)
	}
}

// register registers an agent and returns its id.
func register(t *testing.T, h http.Handler, ws, wt, role string) string {
	t.Helper()
	code, out := doPost(t, h, "/v1/agents/register", map[string]any{
		"workspace_root": ws, "worktree_path": wt, "role": role,
	})
	if code != 200 {
		t.Fatalf("register: code=%d out=%v", code, out)
	}
	return out["agent_id"].(string)
}

func TestRegisterEndpoint(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	first := register(t, h, "/ws", "/ws", "backend")
	if !regexp.MustCompile(`^[a-z]+-[1-9][0-9]{2}$`).MatchString(first) {
		t.Fatalf("id = %s, want word-NNN", first)
	}
	code, out := doPost(t, h, "/v1/agents/register", map[string]any{
		"workspace_root": "/ws", "worktree_path": "/ws/wt-b", "role": "frontend",
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	peers, _ := out["peers"].([]any)
	if out["agent_id"] == first || len(peers) != 1 {
		t.Fatalf("second register out=%v", out)
	}
	if n, _ := out["unread_count"].(float64); n != 0 {
		t.Fatalf("unread_count=%v, want 0", n)
	}
	for _, tc := range []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{
			name:    "missing role",
			body:    map[string]any{"workspace_root": "/ws", "worktree_path": "/ws"},
			wantErr: "bad_request",
		},
		{
			name:    "worktree escape",
			body:    map[string]any{"workspace_root": "/ws", "worktree_path": "/elsewhere", "role": "x"},
			wantErr: "worktree_escape",
		},
		{
			// Relative paths would resolve against the daemon's cwd, not the agent's.
			name:    "relative path",
			body:    map[string]any{"workspace_root": "/ws", "worktree_path": "wt-c", "role": "x"},
			wantErr: "bad_request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doPost(t, h, "/v1/agents/register", tc.body)
			wantErr(t, code, out, tc.wantErr)
		})
	}
	// Resume by name: live names conflict, unknown names 404, offline resumes.
	resume := map[string]any{"workspace_root": "/ws", "worktree_path": "/ws", "role": "lead", "name": first}
	code, out = doPost(t, h, "/v1/agents/register", resume)
	wantErr(t, code, out, "name_in_use")
	doPost(t, h, "/v1/agents/release", map[string]any{"workspace_root": "/ws", "agent_id": first})
	code, out = doPost(t, h, "/v1/agents/register", resume)
	if code != 200 || out["agent_id"] != first {
		t.Fatalf("resume = %d %v, want %s", code, out, first)
	}
	resume["name"] = "nobody-123"
	code, out = doPost(t, h, "/v1/agents/register", resume)
	wantErr(t, code, out, "unknown_agent")
}

func TestReleaseIdempotent(t *testing.T) {
	h := newTestServer(t).Handler()
	code, _ := doPost(t, h, "/v1/agents/release", map[string]any{
		"workspace_root": "/ws", "agent_id": "agent-9",
	})
	if code != 200 {
		t.Fatalf("release unknown code=%d, want 200", code)
	}
	id := register(t, h, "/ws", "/ws", "backend")
	for i := 0; i < 2; i++ {
		code, _ = doPost(t, h, "/v1/agents/release", map[string]any{
			"workspace_root": "/ws", "agent_id": id,
		})
		if code != 200 {
			t.Fatalf("release %d code=%d, want 200", i, code)
		}
	}
}

func TestUpdateEndpoint(t *testing.T) {
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	code, out := doPost(t, h, "/v1/agents/update", map[string]any{
		"workspace_root": "/ws", "agent_id": a1,
		"status": "active", "current_step": "coding", "plan_markdown": "# p",
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	agent, _ := out["agent"].(map[string]any)
	if agent["status"] != "active" || agent["current_step"] != "coding" {
		t.Fatalf("agent=%v", agent)
	}
	if warns, _ := out["warnings"].([]any); len(warns) != 0 {
		t.Fatalf("warnings=%v, want none (no files tracked until phase 3)", warns)
	}
	for _, tc := range []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{
			name:    "unknown agent",
			body:    map[string]any{"workspace_root": "/ws", "agent_id": "agent-9", "status": "active"},
			wantErr: "unknown_agent",
		},
		{
			name:    "invalid status",
			body:    map[string]any{"workspace_root": "/ws", "agent_id": a1, "status": "napping"},
			wantErr: "bad_request",
		},
		{
			name:    "empty update",
			body:    map[string]any{"workspace_root": "/ws", "agent_id": a1},
			wantErr: "bad_request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doPost(t, h, "/v1/agents/update", tc.body)
			wantErr(t, code, out, tc.wantErr)
		})
	}
}

func TestOverlapWarnings(t *testing.T) {
	me := &store.Agent{AgentID: "agent-1", ModifiedFiles: []store.ModifiedFile{{Path: "a.go"}, {Path: "b.go"}}}
	other := &store.Agent{AgentID: "agent-2", ModifiedFiles: []store.ModifiedFile{{Path: "b.go"}, {Path: "c.go"}}}
	w := overlapWarnings(me, []*store.Agent{me, other})
	if len(w) != 1 || w[0].Code != "file_overlap" || len(w[0].Files) != 1 || w[0].Files[0] != "b.go" {
		t.Fatalf("warnings=%+v, want file_overlap [b.go]", w)
	}
	if w := overlapWarnings(me, []*store.Agent{me}); len(w) != 0 {
		t.Fatalf("warnings=%+v, want none", w)
	}
}

func TestSendWakeWait(t *testing.T) {
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	a2 := register(t, h, "/ws", "/ws/wt-b", "frontend")
	type waitRes struct {
		code int
		out  map[string]any
		el   time.Duration
	}
	done := make(chan waitRes, 1)
	go func() {
		start := time.Now()
		code, out := doPost(t, h, "/v1/messages/wait", map[string]any{
			"workspace_root": "/ws", "recipient": a2, "timeout_sec": 5,
		})
		done <- waitRes{code, out, time.Since(start)}
	}()
	time.Sleep(100 * time.Millisecond) // let the waiter park on the hub
	code, out := doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a1, "to": a2, "content": "hi",
	})
	if code != 200 {
		t.Fatalf("send code=%d out=%v", code, out)
	}
	if ids, _ := out["message_ids"].([]any); len(ids) != 1 {
		t.Fatalf("message_ids=%v, want 1", ids)
	}
	res := <-done
	if res.code != 200 {
		t.Fatalf("wait code=%d out=%v", res.code, res.out)
	}
	msgs, _ := res.out["messages"].([]any)
	if len(msgs) != 1 || res.out["timed_out"] != false {
		t.Fatalf("wait out=%v, want 1 message", res.out)
	}
	if m := msgs[0].(map[string]any); m["content"] != "hi" || m["from"] != a1 {
		t.Fatalf("message=%v", m)
	}
	if res.el > 2*time.Second {
		t.Fatalf("wake took %v, want <2s", res.el)
	}
}

func TestWaitTimeout(t *testing.T) {
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws", "backend")
	code, out := doPost(t, h, "/v1/messages/wait", map[string]any{
		"workspace_root": "/ws", "recipient": a1, "timeout_sec": 1,
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["timed_out"] != true {
		t.Fatalf("out=%v, want timed_out", out)
	}
	code, out = doPost(t, h, "/v1/messages/wait", map[string]any{
		"workspace_root": "/ws", "recipient": "agent-9", "timeout_sec": 1,
	})
	wantErr(t, code, out, "unknown_agent")
}

func TestAwaitReply(t *testing.T) {
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	a2 := register(t, h, "/ws", "/ws/wt-b", "frontend")
	doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a2, "to": a1, "content": "old",
	})
	type sendRes struct {
		code int
		out  map[string]any
	}
	done := make(chan sendRes, 1)
	go func() {
		code, out := doPost(t, h, "/v1/messages/send", map[string]any{
			"workspace_root": "/ws", "from": a1, "to": a2,
			"content": "q?", "await_reply": true, "timeout_sec": 5,
		})
		done <- sendRes{code, out}
	}()
	time.Sleep(100 * time.Millisecond)
	doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a2, "to": a1, "content": "new",
	})
	res := <-done
	if res.code != 200 {
		t.Fatalf("send code=%d out=%v", res.code, res.out)
	}
	reply, _ := res.out["reply"].(map[string]any)
	if reply == nil || reply["content"] != "new" || res.out["timed_out"] != false {
		t.Fatalf("out=%v, want reply=new", res.out)
	}
	// The stale message is still unread; the reply was consumed.
	code, out := doPost(t, h, "/v1/messages/take", map[string]any{
		"workspace_root": "/ws", "recipient": a1,
	})
	if code != 200 {
		t.Fatalf("take code=%d out=%v", code, out)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "old" {
		t.Fatalf("take out=%v, want [old]", out)
	}
	// No reply arrives: timeout with null reply.
	code, out = doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a1, "to": a2,
		"content": "q2?", "await_reply": true, "timeout_sec": 1,
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if _, present := out["reply"]; !present || out["reply"] != nil || out["timed_out"] != true {
		t.Fatalf("out=%v, want null reply + timed_out", out)
	}
}

func TestTakeResolve(t *testing.T) {
	h := newTestServer(t).Handler()
	code, out := doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws"})
	wantErr(t, code, out, "no_agents")
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	code, out = doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws"})
	if code != 200 || out["recipient"] != a1 {
		t.Fatalf("code=%d out=%v, want sole %s", code, out, a1)
	}
	register(t, h, "/ws", "/ws/wt-b", "frontend")
	for _, tc := range []struct {
		name      string
		recipient string
		wantErr   string
	}{
		{name: "ambiguous agent", wantErr: "ambiguous_agent"},
		{name: "unknown agent", recipient: "agent-9", wantErr: "unknown_agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"workspace_root": "/ws"}
			if tc.recipient != "" {
				body["recipient"] = tc.recipient
			}
			code, out := doPost(t, h, "/v1/messages/take", body)
			wantErr(t, code, out, tc.wantErr)
		})
	}
}

func TestSendBroadcast(t *testing.T) {
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	a2 := register(t, h, "/ws", "/ws/wt-b", "frontend")
	a3 := register(t, h, "/ws", "/ws/wt-c", "docs")
	code, out := doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a1, "to": "*", "content": "all",
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if ids, _ := out["message_ids"].([]any); len(ids) != 2 {
		t.Fatalf("message_ids=%v, want 2", ids)
	}
	for _, tc := range []struct {
		recipient string
		want      int
	}{{a2, 1}, {a3, 1}, {a1, 0}} {
		t.Run(tc.recipient, func(t *testing.T) {
			code, out := doPost(t, h, "/v1/messages/take", map[string]any{
				"workspace_root": "/ws", "recipient": tc.recipient,
			})
			if code != 200 {
				t.Fatalf("take %s code=%d", tc.recipient, code)
			}
			if msgs, _ := out["messages"].([]any); len(msgs) != tc.want {
				t.Fatalf("take %s = %d, want %d", tc.recipient, len(msgs), tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name       string
		to         string
		awaitReply bool
		wantErr    string
	}{
		{name: "await reply broadcast", to: "*", awaitReply: true, wantErr: "bad_request"},
		{name: "unknown recipient", to: "agent-9", wantErr: "unknown_agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"workspace_root": "/ws",
				"from":           a1,
				"to":             tc.to,
				"content":        "x",
			}
			if tc.awaitReply {
				body["await_reply"] = true
			}
			code, out := doPost(t, h, "/v1/messages/send", body)
			wantErr(t, code, out, tc.wantErr)
		})
	}
}

func TestContained(t *testing.T) {
	for _, tc := range []struct {
		name       string
		root, path string
		want       bool
	}{
		{"same", "/ws", "/ws", true},
		{"sub", "/ws", "/ws/sub", true},
		{"trailing slash", "/ws/", "/ws/sub/", true},
		{"root", "/", "/ws", true},
		{"sibling prefix", "/ws", "/ws2", false},
		{"elsewhere", "/ws", "/elsewhere", false},
		{"parent", "/ws", "/", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contained(tc.root, tc.path); got != tc.want {
				t.Errorf("contained(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}

// MCP hosts kill tool calls at ~60s, so await_reply holds at most
// wait_hold_max_sec per request; /v1/messages/await re-waits after the
// sent message id until the client's budget is spent.
func TestAwaitReplyHoldAndReissue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"wait_hold_max_sec": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	a2 := register(t, h, "/ws", "/ws/wt-b", "frontend")
	start := time.Now()
	code, out := doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a1, "to": a2, "content": "q?",
		"await_reply": true, "timeout_sec": 30,
	})
	if el := time.Since(start); code != 200 || out["timed_out"] != true || el > 3*time.Second {
		t.Fatalf("send = %d %v after %v, want timed_out within the 1s hold", code, out, el)
	}
	sentID := out["message_ids"].([]any)[0].(float64)
	// A reply from someone else, and one predating the send, never count.
	doPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": a2, "to": a1, "content": "the reply"})
	code, out = doPost(t, h, "/v1/messages/await", map[string]any{
		"workspace_root": "/ws", "recipient": a1, "from": a2, "after_id": sentID, "timeout_sec": 5,
	})
	reply, _ := out["reply"].(map[string]any)
	if code != 200 || out["timed_out"] != false || reply["content"] != "the reply" {
		t.Fatalf("await = %d %v, want the reply", code, out)
	}
	code, out = doPost(t, h, "/v1/messages/await", map[string]any{
		"workspace_root": "/ws", "recipient": a1, "from": a2, "after_id": sentID, "timeout_sec": 5,
	})
	if code != 200 || out["timed_out"] != true || out["reply"] != nil {
		t.Fatalf("second await = %d %v, want timed out (reply consumed)", code, out)
	}
	code, out = doPost(t, h, "/v1/messages/await", map[string]any{
		"workspace_root": "/ws", "recipient": "nobody-1", "from": a2, "after_id": sentID,
	})
	wantErr(t, code, out, "unknown_agent")
}

// Hooks cannot name their agent (names are generated), but they run in the
// agent's project directory: take without recipient picks the live agent
// whose worktree contains the hint, when that is unambiguous.
func TestTakeResolvesByWorktree(t *testing.T) {
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	a2 := register(t, h, "/ws", "/ws/wt-b", "frontend")
	doPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": a2, "to": a1, "content": "for a"})
	for _, hint := range []string{"/ws/wt-a", "/ws/wt-a/sub/dir"} {
		t.Run(hint, func(t *testing.T) {
			code, out := doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws", "worktree_path": hint})
			if code != 200 || out["recipient"] != a1 {
				t.Fatalf("hint %s: code=%d out=%v, want %s", hint, code, out, a1)
			}
		})
	}
	// No match, or a hint above both worktrees, stays ambiguous.
	for _, hint := range []string{"/ws", "/elsewhere"} {
		t.Run(hint, func(t *testing.T) {
			code, out := doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws", "worktree_path": hint})
			wantErr(t, code, out, "ambiguous_agent")
		})
	}
	// Two agents in one worktree: ambiguous, pass recipient explicitly.
	register(t, h, "/ws", "/ws/wt-a", "reviewer")
	code, out := doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/wt-a"})
	wantErr(t, code, out, "ambiguous_agent")
}

// registerHost registers an agent with a host PID and returns its id.
func registerHost(t *testing.T, h http.Handler, ws, wt, role string, host int64) string {
	t.Helper()
	code, out := doPost(t, h, "/v1/agents/register", map[string]any{
		"workspace_root": ws, "worktree_path": wt, "role": role, "host_pid": host,
	})
	if code != 200 {
		t.Fatalf("register: code=%d out=%v", code, out)
	}
	return out["agent_id"].(string)
}

// Hooks of two agents started in one folder resolve by the host process
// that spawned each agent's proxy, nearest ancestor first.
func TestResolveByHostPID(t *testing.T) {
	h := newTestServer(t).Handler()
	a := registerHost(t, h, "/ws", "/ws/wt", "arch", 100)
	b := registerHost(t, h, "/ws", "/ws/wt", "build", 200)
	take := func(ancestors []int64) (int, map[string]any) {
		doPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": a, "to": b, "content": "x"})
		doPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": b, "to": a, "content": "y"})
		return doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/wt", "ancestors": ancestors})
	}
	if code, out := take([]int64{7, 200, 1}); code != 200 || out["recipient"] != b {
		t.Fatalf("ancestors 7,200,1: code=%d out=%v, want %s", code, out, b)
	}
	if code, out := take([]int64{100, 200}); code != 200 || out["recipient"] != a {
		t.Fatalf("nearest first: code=%d out=%v, want %s", code, out, a)
	}
	// Hosts gone (or unknown): fall back to folders, which can't decide.
	processAlive = func(int64) bool { return false }
	t.Cleanup(func() { processAlive = defaultProcessAlive })
	code, out := take([]int64{7})
	wantErr(t, code, out, "ambiguous_agent")
	// Both hosts alive and neither is ours: we're another session in the
	// same folder, so we resolve to nobody instead of taking their mail.
	processAlive = func(int64) bool { return true }
	code, out = take([]int64{7})
	wantErr(t, code, out, "no_agents")
}

var defaultProcessAlive = processAlive

// An unregistered session's hook in the sole agent's folder must not
// resolve to that agent while the agent's host is alive.
func TestResolveSkipsForeignLiveHost(t *testing.T) {
	processAlive = func(int64) bool { return true }
	t.Cleanup(func() { processAlive = defaultProcessAlive })
	h := newTestServer(t).Handler()
	a := registerHost(t, h, "/ws", "/ws", "arch", 100)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"take", "/v1/messages/take"},
		{"gate stop", "/v1/gate/stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := doPost(t, h, tc.path, map[string]any{"workspace_root": "/ws", "worktree_path": "/ws", "ancestors": []int64{9, 8}})
			wantErr(t, code, out, "no_agents")
		})
	}
	// Without ancestry (ps failed) folder matching still finds it.
	if code, out := doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws", "worktree_path": "/ws"}); code != 200 || out["recipient"] != a {
		t.Fatalf("no ancestry: %d %v", code, out)
	}
}

// Two agents under one host process can't be told apart by it: fall back
// to folder matching.
func TestResolveSharedHostPIDFallsBack(t *testing.T) {
	h := newTestServer(t).Handler()
	registerHost(t, h, "/ws", "/ws/a", "arch", 100)
	b := registerHost(t, h, "/ws", "/ws/b", "build", 100)
	code, out := doPost(t, h, "/v1/messages/take", map[string]any{"workspace_root": "/ws", "worktree_path": "/ws/b", "ancestors": []int64{100}})
	if code != 200 || out["recipient"] != b {
		t.Fatalf("code=%d out=%v, want folder fallback to %s", code, out, b)
	}
}

// peek: take returns mail without marking it read; ack does that.
func TestTakePeekLeavesUnread(t *testing.T) {
	h := newTestServer(t).Handler()
	a := register(t, h, "/ws", "/ws/a", "x")
	b := register(t, h, "/ws", "/ws/b", "y")
	doPost(t, h, "/v1/messages/send", map[string]any{"workspace_root": "/ws", "from": a, "to": b, "content": "hi"})
	peek := map[string]any{"workspace_root": "/ws", "recipient": b, "peek": true}
	for range 2 {
		if _, out := doPost(t, h, "/v1/messages/take", peek); len(out["messages"].([]any)) != 1 {
			t.Fatalf("peek = %v, want the message both times", out)
		}
	}
	_, out := doPost(t, h, "/v1/messages/take", peek)
	id := out["messages"].([]any)[0].(map[string]any)["id"]
	doPost(t, h, "/v1/messages/ack", map[string]any{"workspace_root": "/ws", "recipient": b, "ids": []any{id}})
	if _, out := doPost(t, h, "/v1/messages/take", peek); len(out["messages"].([]any)) != 0 {
		t.Fatalf("after ack = %v, want none", out)
	}
}
