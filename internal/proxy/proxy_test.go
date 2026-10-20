package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/search-3d/breakroom/internal/paths"
)

// The SDK runs tool calls concurrently, so a host may fire register_agent
// alongside other tools on one session. Run with -race.
func TestSessionConcurrentRegister(t *testing.T) {
	stubDaemon(t, stubMux())
	s := &session{client: NewClient(), workspace: "/ws"}
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, _ = s.handleRegister(ctx, nil, registerArgs{Role: "b", WorktreePath: "/ws"})
		}()
		go func() {
			defer wg.Done()
			_, _, _ = s.handleList(ctx, nil, listArgs{})
		}()
	}
	wg.Wait()
	s.mu.Lock()
	_, ok := s.agents["agent-1"]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("agents = %v, want agent-1", s.agents)
	}
}

// MCP tool calls return after one daemon hold (hosts kill long calls);
// the agent re-issues on timed_out.
func TestWaitIsOneHold(t *testing.T) {
	mux := http.NewServeMux()
	var waits atomic.Int32
	mux.HandleFunc("POST /v1/messages/wait", func(w http.ResponseWriter, _ *http.Request) {
		waits.Add(1)
		_, _ = w.Write([]byte(`{"messages":[],"timed_out":true}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws", agents: map[string]string{"me-100": "/ws"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, env, err := s.handleWait(ctx, nil, waitArgs{TimeoutSec: 300})
	data, _ := env.Data.(map[string]any)
	if err != nil || data["timed_out"] != true || waits.Load() != 1 {
		t.Fatalf("wait = %+v, %v after %d daemon calls; want timed_out after 1", env, err, waits.Load())
	}
}

// AwaitReply is one held request for a reply after the sent message id.
func TestClientAwaitReply(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages/await", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["after_id"] != float64(7) || req["recipient"] != "me-100" || req["from"] != "you-200" || req["timeout_sec"] != float64(30) {
			t.Errorf("await req = %v", req)
		}
		_, _ = w.Write([]byte(`{"reply":{"id":9,"from":"you-200","to":"me-100","type":"chat","content":"yes","created_at":""},"timed_out":false}`))
	})
	stubDaemon(t, mux)
	reply, timedOut, err := NewClient().AwaitReply(context.Background(), "/ws", "me-100", "you-200", 7, 30)
	if err != nil || timedOut || reply == nil || reply.Content != "yes" {
		t.Fatalf("await = %+v %v %v", reply, timedOut, err)
	}
}

// An agent passing "." means its own directory: the proxy (spawned in the
// agent's cwd) makes worktree_path absolute before the daemon sees it.
func TestRegisterResolvesRelativeWorktree(t *testing.T) {
	mux := http.NewServeMux()
	got := make(chan string, 1)
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		got <- req["worktree_path"].(string)
		_, _ = w.Write([]byte(`{"agent_id":"me-100","peers":[],"unread_count":0}`))
	})
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"me-100"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws"}
	if _, env, _ := s.handleRegister(context.Background(), nil, registerArgs{Role: "b", WorktreePath: "./sub"}); !env.OK {
		t.Fatalf("register = %+v", env)
	}
	cwd, _ := os.Getwd()
	if p := <-got; p != filepath.Join(cwd, "sub") {
		t.Fatalf("worktree_path = %q, want %q", p, filepath.Join(cwd, "sub"))
	}
}

// Two bare registers on one session keep both identities live: they are two
// chats sharing one process (e.g. one Cursor window).
func TestSecondRegisterKeepsBoth(t *testing.T) {
	mux := http.NewServeMux()
	var n atomic.Int32
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"me-` + string(rune('0'+n.Add(1))) + `","peers":[],"unread_count":0}`))
	})
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"me"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws"}
	ctx := context.Background()
	_, _, _ = s.handleRegister(ctx, nil, registerArgs{Role: "architect", WorktreePath: "/ws"})
	_, _, _ = s.handleRegister(ctx, nil, registerArgs{Role: "builder", WorktreePath: "/ws"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.agents) != 2 {
		t.Fatalf("agents = %v, want two live identities", s.agents)
	}
}

// Re-registering with caller_id frees the named identity first.
func TestReregisterWithCallerReleasesOld(t *testing.T) {
	mux := http.NewServeMux()
	var n atomic.Int32
	released := make(chan string, 2)
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"me-` + string(rune('0'+n.Add(1))) + `","peers":[],"unread_count":0}`))
	})
	mux.HandleFunc("POST /v1/agents/release", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		released <- req["agent_id"].(string)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"me"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws"}
	ctx := context.Background()
	_, _, _ = s.handleRegister(ctx, nil, registerArgs{Role: "b", WorktreePath: "/ws"})
	_, _, _ = s.handleRegister(withCaller(ctx, "me-1"), nil, registerArgs{Role: "b", WorktreePath: "/ws"})
	select {
	case id := <-released:
		if id != "me-1" {
			t.Fatalf("released %s, want me-1", id)
		}
	default:
		t.Fatal("named identity was not released")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.agents) != 1 {
		t.Fatalf("agents = %v, want only me-2", s.agents)
	}
	if _, ok := s.agents["me-2"]; !ok {
		t.Fatalf("agents = %v, want only me-2", s.agents)
	}
}

// Single-chat hosts re-register without caller_id: by naming their own live
// identity, or bare with the same worktree and role. Either frees the old
// identity first (the daemon only resumes offline ones); a register for a
// different role is a new chat and keeps both.
func TestReregisterWithoutCaller(t *testing.T) {
	for _, c := range []struct {
		name     string
		second   registerArgs
		released string
	}{
		{"name of own identity", registerArgs{Role: "b", WorktreePath: "/ws", Name: "me-1"}, "me-1"},
		{"bare, same worktree and role", registerArgs{Role: "b", WorktreePath: "/ws"}, "me-1"},
		{"bare, other role", registerArgs{Role: "a", WorktreePath: "/ws"}, ""},
		{"bare, other worktree", registerArgs{Role: "b", WorktreePath: "/ws/other"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			mux := http.NewServeMux()
			var n atomic.Int32
			released := make(chan string, 2)
			mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"agent_id":"me-` + string(rune('0'+n.Add(1))) + `","peers":[],"unread_count":0}`))
			})
			mux.HandleFunc("POST /v1/agents/release", func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				released <- req["agent_id"].(string)
				_, _ = w.Write([]byte(`{}`))
			})
			mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"messages":[],"recipient":"me"}`))
			})
			stubDaemon(t, mux)
			s := &session{client: NewClient(), workspace: "/ws"}
			ctx := context.Background()
			_, _, _ = s.handleRegister(ctx, nil, registerArgs{Role: "b", WorktreePath: "/ws"})
			_, _, _ = s.handleRegister(ctx, nil, c.second)
			var got string
			select {
			case got = <-released:
			default:
			}
			if got != c.released {
				t.Fatalf("released %q, want %q", got, c.released)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if want := map[bool]int{true: 1, false: 2}[c.released != ""]; len(s.agents) != want {
				t.Fatalf("agents = %v, want %d", s.agents, want)
			}
		})
	}
}

// A register's workspace comes from its worktree, not the proxy's cwd (Cursor
// starts it in $HOME) or an earlier chat's repo.
func TestRegisterWorkspaceNotShared(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	sent := make(chan string, 2)
	mux := http.NewServeMux()
	var n atomic.Int32
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		sent <- req["workspace_root"].(string)
		_, _ = w.Write([]byte(`{"agent_id":"me-` + string(rune('0'+n.Add(1))) + `","peers":[],"unread_count":0}`))
	})
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"me"}`))
	})
	stubDaemon(t, mux)
	plain := t.TempDir()
	s := &session{client: NewClient(), workspace: t.TempDir()} // the proxy's cwd, e.g. $HOME
	_, _, _ = s.handleRegister(context.Background(), nil, registerArgs{Role: "a", WorktreePath: repo})
	_, _, _ = s.handleRegister(context.Background(), nil, registerArgs{Role: "b", WorktreePath: plain})
	first, second := <-sent, <-sent
	if want := paths.GitCommonRoot(repo); first != want {
		t.Fatalf("repo workspace = %q, want %q", first, want)
	}
	if want := paths.ResolveWorkspace(plain); second != want {
		t.Fatalf("plain-folder workspace = %q, want %q", second, want)
	}
}

// After a client has used caller_id, a call without one could be another
// chat resolved to the sole identity: it must not take that identity's mail.
func TestImplicitCallerGetsNoMailOnceCallerSeen(t *testing.T) {
	var takes atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/", stubMux())
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		takes.Add(1)
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"agent-1"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws",
		agents: map[string]string{"agent-1": "/ws"}}
	ctx := context.Background()
	_, _, _ = s.handleList(ctx, nil, listArgs{})
	if takes.Load() != 1 {
		t.Fatalf("takes = %d before any caller_id, want 1 (single-chat hosts keep mail)", takes.Load())
	}
	_, _, _ = s.handleList(withCaller(ctx, "agent-1"), nil, listArgs{})
	_, env, _ := s.handleList(ctx, nil, listArgs{})
	if !env.OK || takes.Load() != 2 {
		t.Fatalf("implicit call after caller_id: ok=%v takes=%d, want ok and no extra take", env.OK, takes.Load())
	}
}

// With two identities and no caller_id, calls fail instead of guessing.
func TestCallerRequired(t *testing.T) {
	stubDaemon(t, stubMux())
	s := &session{client: NewClient(), workspace: "/ws",
		agents: map[string]string{"mole-311": "/ws", "macaw-624": "/ws"}}
	_, env, _ := s.handleList(context.Background(), nil, listArgs{})
	if env.OK || env.Err != "caller_required: pass caller_id (one of: macaw-624, mole-311)" {
		t.Fatalf("list = %+v, want caller_required naming both", env)
	}
}

// A caller_id this process never registered fails closed.
func TestUnknownCaller(t *testing.T) {
	stubDaemon(t, stubMux())
	s := &session{client: NewClient(), workspace: "/ws",
		agents: map[string]string{"mole-311": "/ws"}}
	ctx := withCaller(context.Background(), "ghost-1")
	_, env, _ := s.handleList(ctx, nil, listArgs{})
	if env.OK || !strings.HasPrefix(env.Err, "unknown_caller:") {
		t.Fatalf("list = %+v, want unknown_caller", env)
	}
}

// One identity and no caller_id behaves as before: the identity acts.
func TestSingleIdentityNeedsNoCaller(t *testing.T) {
	stubDaemon(t, stubMux())
	s := &session{client: NewClient(), workspace: "/ws",
		agents: map[string]string{"solo-1": "/ws"}}
	_, env, _ := s.handleList(context.Background(), nil, listArgs{})
	if !env.OK {
		t.Fatalf("list = %+v, want ok", env)
	}
}

// A call with caller_id acts as that agent: the daemon sees it as the
// caller, and piggybacked mail is taken for it.
func TestCallerActsAsNamedAgent(t *testing.T) {
	mux := http.NewServeMux()
	var from, recipient string
	mux.HandleFunc("POST /v1/messages/send", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		from, _ = req["from"].(string)
		_, _ = w.Write([]byte(`{"message_ids":[7],"timed_out":false}`))
	})
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		recipient, _ = req["recipient"].(string)
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"x"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws",
		agents: map[string]string{"mole-311": "/ws", "macaw-624": "/ws"}}
	ctx := withCaller(context.Background(), "macaw-624")
	_, env, _ := s.handleSend(ctx, nil, sendArgs{ToAgent: "*", Content: "hi"})
	if !env.OK {
		t.Fatalf("send = %+v", env)
	}
	if from != "macaw-624" || recipient != "macaw-624" {
		t.Fatalf("from = %q, take recipient = %q; want macaw-624", from, recipient)
	}
}

// Each identity calls the daemon in the workspace it registered in.
func TestCallerWorkspaceRouting(t *testing.T) {
	mux := http.NewServeMux()
	got := map[string]string{}
	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		got[r.URL.Query().Get("caller")] = r.URL.Query().Get("workspace_root")
		_, _ = w.Write([]byte(`{"agents":[]}`))
	})
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"x"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws",
		agents: map[string]string{"a-1": "/ws-a", "b-1": "/ws-b"}}
	if _, env, _ := s.handleList(withCaller(context.Background(), "a-1"), nil, listArgs{}); !env.OK {
		t.Fatalf("list a-1 = %+v", env)
	}
	if _, env, _ := s.handleList(withCaller(context.Background(), "b-1"), nil, listArgs{}); !env.OK {
		t.Fatalf("list b-1 = %+v", env)
	}
	if got["a-1"] != "/ws-a" || got["b-1"] != "/ws-b" {
		t.Fatalf("workspaces = %v, want per-identity routing", got)
	}
}

// caller_id travels the real MCP path: declared in tools/list, stripped
// before validation, resolved per call.
func TestCallerIDEndToEnd(t *testing.T) {
	mux := http.NewServeMux()
	var n atomic.Int32
	mux.HandleFunc("POST /v1/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agent_id":"me-` + string(rune('0'+n.Add(1))) + `","peers":[],"unread_count":0}`))
	})
	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"agents":[]}`))
	})
	mux.HandleFunc("POST /v1/messages/take", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[],"recipient":"x"}`))
	})
	stubDaemon(t, mux)
	s := &session{client: NewClient(), workspace: "/ws"}
	ctx := context.Background()
	_, _, _ = s.handleRegister(ctx, nil, registerArgs{Role: "architect", WorktreePath: "/ws"})
	_, _, _ = s.handleRegister(ctx, nil, registerArgs{Role: "builder", WorktreePath: "/ws"})

	ct, st := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(s).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	call := func(args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool = %v", err)
		}
		b, _ := json.Marshal(res.StructuredContent)
		return string(b)
	}
	if got := call(nil); !strings.Contains(got, "caller_required") {
		t.Fatalf("no caller_id = %s, want caller_required", got)
	}
	if got := call(map[string]any{"caller_id": "me-1"}); !strings.Contains(got, `"ok":true`) {
		t.Fatalf("caller_id me-1 = %s, want ok", got)
	}
	if got := call(map[string]any{"caller_id": "ghost"}); !strings.Contains(got, "unknown_caller") {
		t.Fatalf("caller_id ghost = %s, want unknown_caller", got)
	}
}
