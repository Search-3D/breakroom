package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/search-3d/breakroom/internal/paths"
)

// stubDaemon serves canned daemon responses on a temp home's socket.
// The home is deliberately short: socket paths die past ~104 chars.
func stubDaemon(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	home, err := os.MkdirTemp("", "br-pxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("BREAKROOM_HOME", home)
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = http.Serve(ln, mux) }()
	t.Cleanup(func() { _ = ln.Close() })
}

// stubMux serves canned daemon replies for the client tests.
func stubMux() *http.ServeMux {
	mux := http.NewServeMux()
	write := func(body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc("POST /v1/agents/register", write(`{"agent_id":"agent-1","peers":[{"agent_id":"agent-2","role":"f","status":"idle","current_step":"s"}],"unread_count":3}`))
	mux.HandleFunc("POST /v1/agents/release", write(`{}`))
	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("workspace_root") != "/ws" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad_request: workspace?"}`))
			return
		}
		_, _ = w.Write([]byte(`{"agents":[{"agent_id":"agent-1","role":"b","status":"active","current_step":"c","modified_files":["a.go"],"last_heartbeat":"2026-10-15T00:00:00Z"}]}`))
	})
	mux.HandleFunc("POST /v1/agents/update", write(`{"agent":{"agent_id":"agent-1","role":"b","status":"active","current_step":"c","modified_files":[],"last_heartbeat":""},"warnings":[{"code":"file_overlap","files":["a.go"]}]}`))
	mux.HandleFunc("POST /v1/messages/send", write(`{"message_ids":[7,8],"reply":{"id":9,"from":"agent-2","to":"agent-1","type":"chat","content":"r","created_at":""},"timed_out":false}`))
	mux.HandleFunc("POST /v1/messages/wait", write(`{"messages":[{"id":7,"from":"agent-1","to":"agent-2","type":"chat","content":"hi","created_at":""}],"timed_out":false}`))
	mux.HandleFunc("POST /v1/messages/take", write(`{"messages":[],"recipient":"agent-1"}`))
	mux.HandleFunc("POST /v1/git/diff", write(`{"diff":"diff --git a/f.txt b/f.txt","files":["f.txt"],"truncated":false,"summary":""}`))
	mux.HandleFunc("POST /v1/git/conflicts", write(`{"conflicts":[{"path":"f.txt","ours_lines":"2","theirs_lines":"2"}],"overlap_only":["g.txt"]}`))
	mux.HandleFunc("POST /v1/messages/compact", write(`{"digest":"DIGEST","message_count":2}`))
	mux.HandleFunc("POST /v1/blueprints/init", write(`{"name":"bp","status":"drafting","builder":"agent-2"}`))
	mux.HandleFunc("POST /v1/blueprints/spec", write(`{"phase":1,"status":"constructing"}`))
	mux.HandleFunc("POST /v1/blueprints/submit", write(`{"phase":1,"status":"inspection"}`))
	mux.HandleFunc("POST /v1/blueprints/inspect", write(`{"phase":1,"status":"punchlist","rounds":1}`))
	mux.HandleFunc("POST /v1/blueprints/close", write(`{"name":"bp","status":"handover","handoff":"handoff-test"}`))
	mux.HandleFunc("POST /v1/blueprints/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Name == "" {
			_, _ = w.Write([]byte(`{"blueprints":[{"name":"bp","architect":"agent-1","builder":"agent-2","goal":"g","status":"active","current_phase":1}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"blueprint":{"name":"bp","architect":"agent-1","builder":"agent-2","goal":"g","status":"active","current_phase":1,"series":true,"worktree_path":"/ws-bp","branch":"breakroom/bp","base_ref":"abc1234","roadmaps":[{"version":1,"phases":[{"phase":1,"title":"t"}],"out_of_scope":"none","base":"main","proposed_at":"2026-10-15T00:00:00Z","approved_via":"chat","approval_quote":"lgtm"}],"phases":[{"phase":1,"title":"t","objective":"o","requirements":[{"id":"r1","text":"x"}],"acceptance":[{"id":"a1","text":"y"}],"constraints":"","status":"inspection","rounds":0,"inspections":[],"updated_at":"2026-10-15T00:00:00Z"}]}}`))
	})
	return mux
}

func TestClientBlueprintRoundTrip(t *testing.T) {
	stubDaemon(t, stubMux())
	c := NewClient()
	ctx := context.Background()

	status, builder, err := c.InitBlueprint(ctx, "/ws", "agent-1", "agent-2", "bp", "g")
	if err != nil || status != "drafting" || builder != "agent-2" {
		t.Fatalf("init = %q %q %v", status, builder, err)
	}
	reqs := []BlueprintRequirement{{ID: "r1", Text: "x"}}
	acc := []BlueprintRequirement{{ID: "a1", Text: "y"}}
	status, err = c.SpecBlueprint(ctx, "/ws", "bp", "agent-1", 1, "t", "o", reqs, acc, "")
	if err != nil || status != "constructing" {
		t.Fatalf("spec = %q %v", status, err)
	}
	status, err = c.SubmitBlueprint(ctx, "/ws", "bp", "agent-2", 1,
		[]BlueprintItem{{RequirementID: "r1", Summary: "done"}}, "ok", "")
	if err != nil || status != "inspection" {
		t.Fatalf("submit = %q %v", status, err)
	}
	status, rounds, err := c.InspectBlueprint(ctx, "/ws", "bp", "agent-1", 1,
		"punchlist", nil, []BlueprintFinding{{RequirementID: "r1", Issue: "i", Fix: "f"}})
	if err != nil || status != "punchlist" || rounds != 1 {
		t.Fatalf("inspect = %q %d %v", status, rounds, err)
	}
	bp, err := c.GetBlueprint(ctx, "/ws", "bp", 0)
	if err != nil || bp.Name != "bp" || len(bp.Phases) != 1 || bp.Phases[0].Requirements[0].ID != "r1" ||
		!bp.Series || bp.WorktreePath != "/ws-bp" || bp.Branch != "breakroom/bp" || bp.BaseRef != "abc1234" ||
		len(bp.Roadmaps) != 1 || bp.Roadmaps[0].Base != "main" {
		t.Fatalf("get = %+v %v", bp, err)
	}
	list, err := c.ListBlueprints(ctx, "/ws")
	if err != nil || len(list) != 1 || list[0].Status != "active" {
		t.Fatalf("list = %+v %v", list, err)
	}
	status, handoff, err := c.CloseBlueprint(ctx, "/ws", "bp", "agent-1")
	if err != nil || status != "handover" || handoff != "handoff-test" {
		t.Fatalf("close = %q %q %v", status, handoff, err)
	}
}

func TestClientRoundTrip(t *testing.T) {
	stubDaemon(t, stubMux())
	c := NewClient()
	ctx := context.Background()

	id, peers, unread, err := c.Register(ctx, "/ws", "/ws", "b", "#p", "test", "", "", 123, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id != "agent-1" || len(peers) != 1 || peers[0].AgentID != "agent-2" || unread != 3 {
		t.Fatalf("register = %s %+v %d", id, peers, unread)
	}
	if err := c.Release(ctx, "/ws", "agent-1"); err != nil {
		t.Fatal(err)
	}
	agents, err := c.ListAgents(ctx, "/ws", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status != "active" || len(agents[0].ModifiedFiles) != 1 {
		t.Fatalf("agents = %+v", agents)
	}
	status := "waiting"
	agent, warnings, err := c.Update(ctx, "/ws", "agent-1", &status, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Status != "active" || len(warnings) != 1 || warnings[0].Code != "file_overlap" {
		t.Fatalf("update = %+v %+v", agent, warnings)
	}
	send, err := c.Send(ctx, SendRequest{Workspace: "/ws", From: "agent-1", To: "agent-2", Content: "q", AwaitReply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(send.MessageIDs) != 2 || send.Reply == nil || send.Reply.Content != "r" || send.TimedOut {
		t.Fatalf("send = %+v", send)
	}
	msgs, timedOut, err := c.Wait(ctx, "/ws", "agent-2", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "hi" || timedOut {
		t.Fatalf("wait = %+v %v", msgs, timedOut)
	}
	msgs, resolved, err := c.Take(ctx, "/ws", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 || resolved != "agent-1" {
		t.Fatalf("take = %+v %q", msgs, resolved)
	}
	diff, err := c.Diff(ctx, "/ws", "agent-1", []string{"f.txt"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Files) != 1 || diff.Truncated || diff.Summary != "" {
		t.Fatalf("diff = %+v", diff)
	}
	conflicts, overlap, err := c.CheckConflicts(ctx, "/ws", "agent-1", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Path != "f.txt" || *conflicts[0].OursLines != "2" {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	if len(overlap) != 1 || overlap[0] != "g.txt" {
		t.Fatalf("overlap = %v", overlap)
	}
	cr, err := c.Compact(ctx, "/ws", 50)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Digest != "DIGEST" || cr.MessageCount != 2 || cr.Fallback {
		t.Fatalf("compact = %+v", cr)
	}
}

func TestClientError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"unknown_agent: no agent-9"}`))
	})
	stubDaemon(t, mux)
	_, _, err := NewClient().Take(context.Background(), "/ws", "agent-9", "", nil)
	var derr *Error
	if !errors.As(err, &derr) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if derr.Code != "unknown_agent" || derr.Detail != "no agent-9" || derr.StatusCode != 404 {
		t.Fatalf("err = %+v", derr)
	}
}

func TestClientUnreachable(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	_, _, err := NewClient().Take(context.Background(), "/ws", "agent-1", "", nil)
	var derr *Error
	if !errors.As(err, &derr) || derr.Code != "daemon_unreachable" {
		t.Fatalf("err = %v, want daemon_unreachable", err)
	}
}
