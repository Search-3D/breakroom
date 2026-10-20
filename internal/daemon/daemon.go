// Package daemon manages the background broker process: single-instance
// locking, detached spawn, a Unix-socket HTTP server, and idle shutdown.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/store"
	"golang.org/x/sys/unix"
)

// ErrNotRunning is returned when no daemon holds the instance lock.
var ErrNotRunning = errors.New("daemon is not running")

var errLocked = errors.New("pidfile is locked")

// Start spawns a detached daemon, waits until it serves status, and returns
// its PID. The child-side pidfile lock is the real single-instance guard, so
// two racing starters converge on one daemon.
func Start(idle time.Duration) (int, error) {
	up, err := daemonUp()
	if err != nil {
		return 0, err
	}
	if up {
		return 0, fmt.Errorf("breakroom: already running (pid %d)", readPid())
	}
	if err := os.MkdirAll(paths.Dir(), 0o700); err != nil {
		return 0, err
	}
	logFile, err := openLog()
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(self, "start", "--foreground", "--idle-timeout", idle.String())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = paths.Dir() // never pin the caller's cwd (deleted dirs, unmounts)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("breakroom: spawn daemon: %w", err)
	}
	_ = cmd.Process.Release()
	if err := waitForReady(5 * time.Second); err != nil {
		return 0, fmt.Errorf("breakroom: daemon did not become ready: %w (see %s)", err, paths.Log())
	}
	return readPid(), nil
}

// EnsureStarted returns the daemon PID, starting it first if needed.
func EnsureStarted(idle time.Duration) (int, error) {
	info, err := QueryStatus()
	if err == nil {
		return info.PID, nil
	}
	if errors.Is(err, ErrNotRunning) {
		pid, serr := Start(idle)
		if serr == nil {
			return pid, nil
		}
		err = serr
	}
	// Another starter's daemon holds the lock but may not have bound the
	// socket yet (store migrations run first). Wait for it, don't fail.
	if up, _ := daemonUp(); !up || waitForReady(5*time.Second) != nil {
		return 0, err
	}
	return readPid(), nil
}

// Stop shuts the daemon down, returning false if it wasn't running. The
// daemon removes its own socket and pidfile on exit.
func Stop() (bool, error) {
	return stop(0)
}

// StopIf stops the daemon only if it is still pid: a caller that decided
// from an older status (say, to swap out a stale version) must not kill a
// fresh daemon another process started in the meantime.
func StopIf(pid int) (bool, error) {
	return stop(pid)
}

// stop terminates the running daemon; want > 0 limits it to that pid.
func stop(want int) (bool, error) {
	up, err := daemonUp()
	if err != nil || !up {
		return false, err
	}
	// The pidfile is briefly empty while a starting daemon writes it; pid 0
	// would signal our own process group.
	pid := readPid()
	for i := 0; pid <= 0 && i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		pid = readPid()
	}
	if pid <= 0 {
		return true, errors.New("breakroom: daemon is running but its pid is unknown; try again")
	}
	if want > 0 && pid != want {
		return false, nil // already replaced
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(unix.SIGTERM)
	}
	if down, err := waitForDown(5 * time.Second); err != nil || down {
		return true, err
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(unix.SIGKILL)
	}
	down, err := waitForDown(2 * time.Second)
	if err != nil {
		return true, err
	}
	if !down {
		return true, fmt.Errorf("breakroom: pid %d did not exit", pid)
	}
	// Killed, so it couldn't clean up; unless a new daemon already took over.
	if up, err := daemonUp(); err == nil && !up {
		cleanupFiles()
	}
	return true, nil
}

// QueryStatus asks a running daemon for status.
func QueryStatus() (*Info, error) {
	up, err := daemonUp()
	if err != nil {
		return nil, err
	}
	if !up {
		return nil, ErrNotRunning
	}
	resp, err := unixClient().Get("http://localhost/v1/status")
	if err != nil {
		return nil, fmt.Errorf("breakroom: daemon not responding: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("breakroom: status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var info Info
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("breakroom: bad status response: %w", err)
	}
	return &info, nil
}

// RunForeground runs the daemon in-process until a signal or idle timeout.
func RunForeground(idle time.Duration) error {
	if err := os.MkdirAll(paths.Dir(), 0o700); err != nil {
		return err
	}
	logFile, err := openLog()
	if err != nil {
		return err
	}
	defer logFile.Close()
	log := slog.New(slog.NewTextHandler(logFile, nil))
	// Lock first: a second instance must not migrate the live daemon's
	// database before finding out it lost.
	lock, err := lockPid()
	if err != nil {
		if errors.Is(err, errLocked) {
			return fmt.Errorf("breakroom: already running (pid %d)", readPid())
		}
		return err
	}
	defer lock.Close() // releasing the lock marks us stopped
	if err := writePid(lock); err != nil {
		return err
	}
	st, err := store.Open(paths.DB())
	if err != nil {
		return err
	}
	defer st.Close()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	_ = os.Remove(paths.Socket())
	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		return fmt.Errorf("breakroom: listen %s: %w", paths.Socket(), err)
	}
	srv := newServer(st, cfg)
	srv.log = log
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// A bad tcp_listen loses only TCP; the Unix socket every MCP proxy uses
	// keeps serving.
	tcpCleanup, err := EnsureTCP(cfg.TCPListen, srv.Handler(), log)
	if err != nil {
		log.Warn("tcp listener disabled", "addr", cfg.TCPListen, "error", err)
		srv.tcpAddr = ""
	} else {
		defer tcpCleanup()
	}
	go func() { _ = httpSrv.Serve(ln) }()
	log.Info("daemon started", "pid", os.Getpid(), "socket", paths.Socket(), "idle_timeout", idle.String())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, unix.SIGINT, unix.SIGTERM)
	defer signal.Stop(sig)
	// Reap and idle-check every 10s, faster for short idle timeouts so
	// --idle-timeout 1s means about 1s.
	tick := 10 * time.Second
	if idle > 0 {
		tick = min(tick, max(idle/4, 100*time.Millisecond))
	}
	idleTick := time.NewTicker(tick)
	defer idleTick.Stop()
	var lastPrune time.Time
	for {
		if time.Since(lastPrune) >= time.Hour {
			lastPrune = time.Now()
			prune(st, cfg.RetentionDays, log)
		}
		select {
		case s := <-sig:
			log.Info("shutdown on signal", "signal", s.String())
			return shutdown(httpSrv, ln, log)
		case <-idleTick.C:
			reapDeadProxies(st, log)
			srv.forgetOffline(context.Background())
			if idle <= 0 {
				continue
			}
			present, err := st.CountPresent(context.Background())
			if err != nil {
				log.Error("idle check failed", "error", err)
				continue
			}
			if present == 0 && srv.idleFor() > idle {
				log.Info("idle shutdown", "idle_for", srv.idleFor().String())
				return shutdown(httpSrv, ln, log)
			}
		}
	}
}

// prune drops history older than the retention window (days < 0: never).
func prune(st *store.Store, days int, log *slog.Logger) {
	if days < 0 {
		return
	}
	msgs, bps, err := st.Prune(context.Background(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		log.Error("prune failed", "error", err)
	} else if msgs > 0 || bps > 0 {
		log.Info("pruned history", "retention_days", days, "messages", msgs, "blueprints", bps)
	}
}

// maxLogBytes caps daemon.log: past it, the next start moves it to
// daemon.log.1 (replacing the previous one) and begins a fresh log.
const maxLogBytes = 10 << 20

// openLog opens daemon.log for appending, first moving it to daemon.log.1 if
// it grew past the size cap.
func openLog() (*os.File, error) {
	if fi, err := os.Stat(paths.Log()); err == nil && fi.Size() > maxLogBytes {
		_ = os.Rename(paths.Log(), paths.Log()+".1")
	}
	return os.OpenFile(paths.Log(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

// reapDeadProxies releases agents whose proxy PID is gone (proxies that died
// without a clean release, e.g. SIGKILL).
func reapDeadProxies(st *store.Store, log *slog.Logger) {
	agents, err := st.ListOnlineAgents(context.Background())
	if err != nil {
		log.Error("reap failed", "error", err)
		return
	}
	for _, a := range agents {
		if a.PID == nil {
			continue
		}
		err := unix.Kill(int(*a.PID), 0)
		if err == nil || errors.Is(err, unix.EPERM) {
			continue // alive (EPERM means a live process we may not signal)
		}
		if !errors.Is(err, unix.ESRCH) {
			log.Error("pid check failed", "agent", a.AgentID, "pid", *a.PID, "error", err)
			continue
		}
		log.Info("reaping dead proxy", "agent", a.AgentID, "workspace", a.WorkspaceRoot, "pid", *a.PID)
		if err := st.ReleaseAgent(context.Background(), a.WorkspaceRoot, a.AgentID); err != nil {
			log.Error("release failed", "error", err)
		}
	}
}

// shutdown stops the HTTP server, closes the listener, and removes the socket,
// pidfile, and token.
func shutdown(httpSrv *http.Server, ln net.Listener, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	_ = ln.Close()
	cleanupFiles()
	log.Info("daemon stopped")
	return nil
}

// unixClient is a short-timeout HTTP client over the daemon's Unix socket.
func unixClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", paths.Socket())
			},
		},
	}
}

// waitForReady polls until the daemon answers, or timeout passes.
func waitForReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := QueryStatus(); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timed out waiting for status")
}

// waitForDown polls until the daemon is gone, reporting whether it went down
// within timeout.
func waitForDown(timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		up, err := daemonUp()
		if err != nil {
			return false, err
		}
		if !up {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, nil
}

// lockPid opens the pidfile and takes an exclusive non-blocking flock.
// The caller must hold the returned file open for as long as the daemon runs.
func lockPid() (*os.File, error) {
	if err := os.MkdirAll(paths.Dir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(paths.Pid(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("breakroom: lock %s: %w", paths.Pid(), err)
	}
	return f, nil
}

// daemonUp reports whether another process holds the pidfile lock.
func daemonUp() (bool, error) {
	f, err := os.OpenFile(paths.Pid(), os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("breakroom: lock %s: %w", paths.Pid(), err)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false, nil
}

// writePid replaces the pidfile's contents with this process's PID.
func writePid(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := f.WriteString(strconv.Itoa(os.Getpid()))
	return err
}

// readPid returns the PID in the pidfile, or 0.
func readPid() int {
	b, err := os.ReadFile(paths.Pid())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// cleanupFiles removes the socket, pidfile, and token.
func cleanupFiles() {
	_ = os.Remove(paths.Socket())
	_ = os.Remove(paths.Pid())
	_ = os.Remove(paths.Token())
}
