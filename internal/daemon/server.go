package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/store"
	"github.com/search-3d/breakroom/internal/version"
)

// Info describes a running daemon.
type Info struct {
	Version       string      `json:"version"`
	PID           int         `json:"pid"`
	StartedAt     time.Time   `json:"started_at"`
	UptimeSec     int64       `json:"uptime_sec"`
	AgentsPresent int         `json:"agents_present"`
	AgentsDone    int         `json:"agents_completed"` // connected but completed; not present
	Agents        []AgentInfo `json:"agents"`           // every connected agent, all workspaces
	Socket        string      `json:"socket"`
	TCPAddr       string      `json:"tcp_addr"` // "" when TCP is disabled
}

// AgentInfo is one connected agent in the daemon status.
type AgentInfo struct {
	WorkspaceRoot string `json:"workspace_root"`
	AgentID       string `json:"agent_id"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	WorktreePath  string `json:"worktree_path"`
}

type server struct {
	store         *store.Store
	log           *slog.Logger
	hub           *hub
	tcpAddr       string
	startedAt     time.Time
	mu            sync.Mutex
	lastAct       time.Time
	mailRuns      map[string]int       // gate: immediate-mail stops per agent this turn
	parked        map[string]int       // gate: count of active s.park holds by hubKey
	lastCallAt    map[string]time.Time // hubKey(ws, agentID) -> timestamp of last call
	lastGateAgent map[string]string    // ws -> agentID of most recent gate stop
	lastGateAt    map[string]time.Time // ws -> timestamp of most recent gate stop
}

// newServer returns a server using st, with its in-memory state empty.
func newServer(st *store.Store, cfg Config) *server {
	now := time.Now()
	return &server{
		store: st, hub: newHub(), log: slog.Default(),
		tcpAddr:   cfg.TCPListen,
		startedAt: now, lastAct: now, mailRuns: map[string]int{},
		parked:        map[string]int{},
		lastCallAt:    map[string]time.Time{},
		lastGateAgent: map[string]string{},
		lastGateAt:    map[string]time.Time{},
	}
}

// isParked reports whether the agent is being held by the stop gate right now.
func (s *server) isParked(ws, agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parked[hubKey(ws, agent)] > 0
}

// forgetOffline drops per-agent gate state (mail runs, last call times) for
// agents no longer online, so the maps don't grow until an idle restart.
func (s *server) forgetOffline(ctx context.Context) {
	agents, err := s.store.ListOnlineAgents(ctx)
	if err != nil {
		return
	}
	online := map[string]bool{}
	for _, a := range agents {
		online[hubKey(a.WorkspaceRoot, a.AgentID)] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.mailRuns {
		if !online[k] {
			delete(s.mailRuns, k)
		}
	}
	for k := range s.lastCallAt {
		if !online[k] {
			delete(s.lastCallAt, k)
		}
	}
}

// touchCaller records that the agent just called a Breakroom tool.
func (s *server) touchCaller(ws, agent string) {
	if ws == "" || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCallAt[hubKey(ws, agent)] = time.Now()
}

// touchGate records that a stop hook just resolved to the agent.
func (s *server) touchGate(ws, agent string) {
	if ws == "" || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastGateAgent[ws] = agent
	s.lastGateAt[ws] = time.Now()
}

// idleFor is how long since the daemon last saw activity.
func (s *server) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastAct)
}

// Handler returns the daemon's HTTP routes. One mux serves the Unix socket
// and, when configured, the localhost TCP listener (behind bearer auth).
func (s *server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/agents/register", s.handleRegister)
	mux.HandleFunc("POST /v1/agents/release", s.handleRelease)
	mux.HandleFunc("GET /v1/agents", s.handleListAgents)
	mux.HandleFunc("POST /v1/agents/update", s.handleUpdate)
	mux.HandleFunc("POST /v1/messages/send", s.handleSend)
	mux.HandleFunc("POST /v1/messages/wait", s.handleWait)
	mux.HandleFunc("POST /v1/messages/await", s.handleAwait)
	mux.HandleFunc("POST /v1/messages/take", s.handleTake)
	mux.HandleFunc("POST /v1/messages/compact", s.handleCompact)
	mux.HandleFunc("POST /v1/git/diff", s.handleDiff)
	mux.HandleFunc("POST /v1/git/conflicts", s.handleConflicts)
	mux.HandleFunc("POST /v1/blueprints/init", s.handleBlueprintInit)
	mux.HandleFunc("POST /v1/blueprints/spec", s.handleBlueprintSpec)
	mux.HandleFunc("POST /v1/blueprints/submit", s.handleBlueprintSubmit)
	mux.HandleFunc("POST /v1/blueprints/inspect", s.handleBlueprintInspect)
	mux.HandleFunc("POST /v1/blueprints/status", s.handleBlueprintStatus)
	mux.HandleFunc("POST /v1/blueprints/close", s.handleBlueprintClose)
	mux.HandleFunc("POST /v1/blueprints/roadmap", s.handleBlueprintRoadmap)
	mux.HandleFunc("POST /v1/blueprints/approve", s.handleBlueprintApprove)
	mux.HandleFunc("POST /v1/blueprints/join", s.handleBlueprintJoin)
	mux.HandleFunc("POST /v1/blueprints/series", s.handleBlueprintSeries)
	mux.HandleFunc("POST /v1/blueprints/acceptance", s.handleBlueprintAcceptance)
	mux.HandleFunc("POST /v1/blueprints/verdict", s.handleBlueprintVerdict)
	mux.HandleFunc("POST /v1/blueprints/land", s.handleBlueprintLand)
	mux.HandleFunc("POST /v1/prompts/record", s.handlePromptRecord)
	mux.HandleFunc("POST /v1/messages/ack", s.handleAck)
	mux.HandleFunc("POST /v1/gate/stop", s.handleGateStop)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Status is observation, not activity: a statusline or health check
		// polling it must not keep an unused daemon alive forever.
		if r.URL.Path != "/v1/status" {
			s.mu.Lock()
			s.lastAct = time.Now()
			s.mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	})
}

// handleStatus reports the daemon's version, uptime, and connected agents.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	present, err := s.store.CountPresent(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	done, err := s.store.CountCompleted(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	online, err := s.store.ListOnlineAgents(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	agents := []AgentInfo{}
	for _, a := range online {
		agents = append(agents, AgentInfo{a.WorkspaceRoot, a.AgentID, a.Role, a.Status, a.WorktreePath})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Info{
		Version:       version.Version,
		PID:           os.Getpid(),
		StartedAt:     s.startedAt,
		UptimeSec:     int64(time.Since(s.startedAt).Seconds()),
		AgentsPresent: present,
		AgentsDone:    done,
		Agents:        agents,
		Socket:        paths.Socket(),
		TCPAddr:       s.tcpAddr,
	})
}
