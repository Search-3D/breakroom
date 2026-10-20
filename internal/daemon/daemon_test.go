package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/store"
)

// openStore opens a store on a fresh temp database.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestStatusHandler(t *testing.T) {
	srv := newServer(openStore(t), defaultConfig())
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var info Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.PID != os.Getpid() || info.Version == "" || info.StartedAt.IsZero() {
		t.Fatalf("info = %+v", info)
	}
	if info.AgentsPresent != 0 {
		t.Fatalf("agents = %d, want 0", info.AgentsPresent)
	}
	id := register(t, srv.Handler(), "/ws", "/ws/a", "architect")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	info = Info{}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if len(info.Agents) != 1 || info.Agents[0].AgentID != id || info.Agents[0].Role != "architect" || info.Agents[0].WorkspaceRoot != "/ws" {
		t.Fatalf("status agents = %+v", info.Agents)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestPidLockSingleInstance(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	first, err := lockPid()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := lockPid(); !errors.Is(err, errLocked) {
		t.Fatalf("second lock err = %v, want errLocked", err)
	}
	if up, err := daemonUp(); err != nil || !up {
		t.Fatalf("daemonUp = %v, %v; want true, nil", up, err)
	}
	first.Close()
	if up, err := daemonUp(); err != nil || up {
		t.Fatalf("after close daemonUp = %v, %v; want false, nil", up, err)
	}
	relock, err := lockPid()
	if err != nil {
		t.Fatal(err)
	}
	relock.Close()
}

// Stop never touches files it doesn't own: with no daemon up, a socket
// and pidfile belong to one that is starting right now.
func TestStopLeavesStartingDaemonFiles(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	for _, p := range []string{paths.Socket(), paths.Pid()} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if running, err := Stop(); err != nil || running {
		t.Fatalf("Stop = %v, %v; want false, nil", running, err)
	}
	for _, p := range []string{paths.Socket(), paths.Pid()} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
}

// A locked but still-empty pidfile (a daemon mid-start) must not make Stop
// signal pid 0, which is our own process group.
func TestStopEmptyPidfile(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	lock, err := lockPid()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := Stop(); err == nil || !strings.Contains(err.Error(), "pid is unknown") {
		t.Fatalf("Stop err = %v, want pid unknown", err)
	}
}

// StopIf leaves a daemon alone once it isn't the pid the caller saw: two
// proxies swapping a stale daemon must not kill the fresh one.
func TestStopIfReplaced(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	lock, err := lockPid()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writePid(lock); err != nil { // "the daemon" is this test process
		t.Fatal(err)
	}
	if stopped, err := StopIf(os.Getpid() + 1); err != nil || stopped {
		t.Fatalf("StopIf(other pid) = %v, %v; want false, nil (no signal)", stopped, err)
	}
}

// A daemon another process just started holds the lock before it binds the
// socket. EnsureStarted must wait for it, not fail: Cursor spawns the MCP
// proxy alongside the sessionStart hook and never retries a failed server.
func TestEnsureStartedWaitsForStartingDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "br-ens-") // short: unix socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("BREAKROOM_HOME", home)
	lock, err := lockPid()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := writePid(lock); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond) // store migrations, config load
		ln, err := net.Listen("unix", paths.Socket())
		if err != nil {
			return
		}
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(Info{PID: os.Getpid()})
		}))
	}()
	pid, err := EnsureStarted(time.Minute)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("EnsureStarted = %d, %v; want %d, nil", pid, err, os.Getpid())
	}
}

// A proxy killed without releasing (SIGKILL, host crash) must not hold its
// identity forever: the reaper marks it offline so it can be resumed.
func TestReapDeadProxies(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Skip("cannot spawn a process:", err)
	}
	deadPID, livePID := int64(dead.Process.Pid), int64(os.Getpid())
	gone, err := st.RegisterAgent(ctx, store.RegisterParams{WorkspaceRoot: "/ws", WorktreePath: "/ws/a", Role: "a", PID: &deadPID})
	if err != nil {
		t.Fatal(err)
	}
	alive, err := st.RegisterAgent(ctx, store.RegisterParams{WorkspaceRoot: "/ws", WorktreePath: "/ws/b", Role: "b", PID: &livePID})
	if err != nil {
		t.Fatal(err)
	}
	// A finished agent whose host killed it stays "completed" without a
	// release; unreaped, hooks in its worktree would see two live agents.
	done, err := st.RegisterAgent(ctx, store.RegisterParams{WorkspaceRoot: "/ws", WorktreePath: "/ws/c", Role: "c", PID: &deadPID})
	if err != nil {
		t.Fatal(err)
	}
	completed := "completed"
	if _, err := st.UpdateAgent(ctx, "/ws", done.AgentID, store.AgentUpdate{Status: &completed}); err != nil {
		t.Fatal(err)
	}
	reapDeadProxies(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		id, want string
	}{{gone.AgentID, "offline"}, {done.AgentID, "offline"}, {alive.AgentID, "idle"}} {
		t.Run(tc.id, func(t *testing.T) {
			a, err := st.GetAgent(ctx, "/ws", tc.id)
			if err != nil || a.Status != tc.want {
				t.Fatalf("%s status = %v (%v), want %s", tc.id, a, err, tc.want)
			}
		})
	}
}

// An oversized daemon.log moves to daemon.log.1 when the daemon starts.
func TestOpenLogRotates(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	if err := os.WriteFile(paths.Log(), make([]byte, maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLog()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, err := os.Stat(paths.Log()); err != nil || fi.Size() != 0 {
		t.Fatalf("fresh log: %v %v", fi, err)
	}
	if fi, err := os.Stat(paths.Log() + ".1"); err != nil || fi.Size() != maxLogBytes+1 {
		t.Fatalf("rotated log: %v %v", fi, err)
	}
}

// Released agents' gate state is dropped on the reap tick; live agents keep it.
func TestForgetOffline(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	a := register(t, h, "/ws", "/ws/a", "architect")
	b := register(t, h, "/ws", "/ws/b", "builder")
	s.touchCaller("/ws", a)
	s.touchCaller("/ws", b)
	s.mailRun(hubKey("/ws", a), false, true)
	doPost(t, h, "/v1/agents/release", map[string]any{"workspace_root": "/ws", "agent_id": a})
	s.forgetOffline(context.Background())
	_, aCall := s.lastCallAt[hubKey("/ws", a)]
	_, aRun := s.mailRuns[hubKey("/ws", a)]
	_, bCall := s.lastCallAt[hubKey("/ws", b)]
	if aCall || aRun || !bCall {
		t.Fatalf("after forget: a call=%v run=%v, b call=%v", aCall, aRun, bCall)
	}
}
