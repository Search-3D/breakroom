package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildBinary compiles breakroom into a temp dir and returns its path.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "breakroom")
	args := []string{"build", "-o", bin}
	// GOCOVERDIR set: instrument the binary so e2e runs count toward
	// coverage (go tool covdata textfmt -i=$GOCOVERDIR -o=e2e.out).
	if os.Getenv("GOCOVERDIR") != "" {
		args = append(args, "-cover", "-coverpkg=github.com/search-3d/breakroom/...")
	}
	cmd := exec.Command("go", append(args, ".")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// runBin runs the binary with BREAKROOM_HOME set, returning output and exit code.
func runBin(t *testing.T, home, bin string, args ...string) (string, int) {
	t.Helper()
	return runIn(t, home, bin, "", args...)
}

// runIn is runBin run from dir.
func runIn(t *testing.T, home, bin, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "BREAKROOM_HOME="+home)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("run %v: %v", args, err)
	return "", -1
}

func TestDaemonLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e lifecycle test")
	}
	bin := buildBinary(t)
	// Short temp path: Unix socket paths have a ~104-char limit.
	home, err := os.MkdirTemp("", "br-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run LIFO: stop first while home (pidfile) still exists.
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })

	if out, code := runBin(t, home, bin, "status"); code == 0 {
		t.Fatalf("status before start = 0, want nonzero (%s)", out)
	}
	out, code := runBin(t, home, bin, "start")
	if code != 0 {
		t.Fatalf("start: %s", out)
	}
	if !strings.Contains(out, "started (pid ") {
		t.Fatalf("start out = %q", out)
	}
	if out, code := runBin(t, home, bin, "start"); code == 0 {
		t.Fatalf("second start = 0, want already-running error (%s)", out)
	}
	out, code = runBin(t, home, bin, "status")
	if code != 0 {
		t.Fatalf("status: %s", out)
	}
	if !strings.Contains(out, "agents present: 0") {
		t.Fatalf("status out = %q", out)
	}
	out, code = runBin(t, home, bin, "status", "--format", "json")
	if code != 0 {
		t.Fatalf("status json: %s", out)
	}
	if !strings.Contains(out, `"ok":true`) || !strings.Contains(out, `"agents_present":0`) {
		t.Fatalf("status json = %q", out)
	}
	out, code = runBin(t, home, bin, "stop")
	if code != 0 || !strings.Contains(out, "stopped") {
		t.Fatalf("stop = %d %q", code, out)
	}
	if out, code := runBin(t, home, bin, "status"); code == 0 {
		t.Fatalf("status after stop = 0 (%s)", out)
	}
	out, code = runBin(t, home, bin, "doctor")
	if code != 0 || !strings.Contains(out, "not running") {
		t.Fatalf("doctor = %d %q", code, out)
	}
	out, code = runBin(t, home, bin, "logs")
	if code != 0 || !strings.Contains(out, "daemon started") {
		t.Fatalf("logs = %d %q", code, out)
	}
	for _, f := range []string{"daemon.log.1", "trace-123.log"} {
		if err := os.WriteFile(filepath.Join(home, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, code = runBin(t, home, bin, "prune")
	if code != 0 || !strings.Contains(out, "pruned") {
		t.Fatalf("prune = %d %q", code, out)
	}
	for _, f := range []string{"state.db", "daemon.log", "daemon.log.1", "trace-123.log"} {
		if _, err := os.Stat(filepath.Join(home, f)); !os.IsNotExist(err) {
			t.Fatalf("%s survived prune: %v", f, err)
		}
	}
}

// --idle-timeout really shuts an unused daemon down.
func TestIdleShutdown(t *testing.T) {
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-idle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	if out, code := runBin(t, home, bin, "start", "--idle-timeout", "1s"); code != 0 {
		t.Fatalf("start = %d %q", code, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, code := runBin(t, home, bin, "status"); code != 0 {
			return // shut itself down
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("daemon still running 10s after a 1s idle timeout")
}

// CLI failure paths: usage, JSON errors with no daemon, unreachable sends.
func TestCLIErrors(t *testing.T) {
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-err-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if out, code := runBin(t, home, bin, "frobnicate"); code != 2 || !strings.Contains(out, "unknown command") || !strings.Contains(out, "Commands:") {
		t.Fatalf("unknown command = %d %q", code, out)
	}
	if out, code := runBin(t, home, bin, "help"); code != 0 || !strings.Contains(out, "Commands:") {
		t.Fatalf("help = %d %q", code, out)
	}
	out, code := runBin(t, home, bin, "status", "--format", "json")
	var env struct {
		OK  bool   `json:"ok"`
		Err string `json:"error"`
	}
	if code != 1 || json.Unmarshal([]byte(out), &env) != nil || env.OK || !strings.HasPrefix(env.Err, "not_running:") {
		t.Fatalf("status json = %d %q, want not_running envelope", code, out)
	}
	// The CLI never spawns a daemon behind your back (hooks would).
	if out, code := runBin(t, home, bin, "send", "--from", "x-100", "--to", "y-200", "--message", "hi"); code != 1 || !strings.Contains(out, "daemon_unreachable") {
		t.Fatalf("send without daemon = %d %q", code, out)
	}
	if _, code := runBin(t, home, bin, "status"); code == 0 {
		t.Fatal("CLI send started a daemon")
	}
}

func TestDoctorReportWithPlugins(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Before install: no plugins
	rep := doctorReport(context.Background())
	if len(rep.Plugins) != 0 {
		t.Fatalf("expected 0 plugins in clean home, got %d", len(rep.Plugins))
	}

	// Install a plugin globally
	code := runInstall(context.Background(), []string{"--host", "cursor", "--global"})
	if code != 0 {
		t.Fatalf("runInstall returned %d", code)
	}

	rep = doctorReport(context.Background())
	found := false
	for _, p := range rep.Plugins {
		if p.Host == "cursor" {
			found = true
			if !strings.Contains(p.Status, "installed") {
				t.Fatalf("cursor status = %q, want installed", p.Status)
			}
		}
	}
	if !found {
		t.Fatalf("doctorReport did not report installed cursor plugin: %+v", rep.Plugins)
	}
}
