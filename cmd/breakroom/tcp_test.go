package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// freePort returns a free loopback address to listen on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// tcpGet calls /v1/status over TCP with the bearer token, returning status and body.
func tcpGet(t *testing.T, addr, token string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("tcp get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestTCPListener(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tcp test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-tcp-")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run LIFO: stop first while home (pidfile) still exists.
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Cleanup(func() { runBin(t, home, bin, "stop") })

	addr := "127.0.0.1:" + freePort(t)
	cfg := `{"tcp_listen": "` + addr + `"}`
	if err := os.WriteFile(home+"/config.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := runBin(t, home, bin, "start"); code != 0 {
		t.Fatalf("start: %s", out)
	}
	token, err := os.ReadFile(home + "/token")
	if err != nil {
		t.Fatal(err)
	}
	// TCP binds alongside UDS; poll until it answers with auth.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _ := tcpGet(t, addr, strings.TrimSpace(string(token)))
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tcp never came up (last code %d)", code)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, body := tcpGet(t, addr, ""); code != 401 || !strings.Contains(body, "unauthorized") {
		t.Fatalf("no token = %d %q, want 401", code, body)
	}
	if code, _ := tcpGet(t, addr, "wrong"); code != 401 {
		t.Fatalf("bad token = %d, want 401", code)
	}
	code, body := tcpGet(t, addr, strings.TrimSpace(string(token)))
	if code != 200 || !strings.Contains(body, `"tcp_addr":"`+addr+`"`) {
		t.Fatalf("authed = %d %q", code, body)
	}
	out, code := runBin(t, home, bin, "status")
	if code != 0 || !strings.Contains(out, "tcp: "+addr) {
		t.Fatalf("status = %d %q, want tcp line", code, out)
	}
	if out, code := runBin(t, home, bin, "stop"); code != 0 {
		t.Fatalf("stop: %s", out)
	}
	if _, err := os.Stat(home + "/token"); !os.IsNotExist(err) {
		t.Fatalf("token file survives stop")
	}
}

func TestTCPRejectsNonLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tcp test")
	}
	bin := buildBinary(t)
	home, err := os.MkdirTemp("", "br-tcpbad-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	port := freePort(t)
	cfg := fmt.Sprintf(`{"tcp_listen": "0.0.0.0:%s"}`, port)
	if err := os.WriteFile(home+"/config.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runBin(t, home, bin, "stop") })
	// The daemon still serves its Unix socket (MCP keeps working), but
	// nothing listens on the rejected address.
	if out, code := runBin(t, home, bin, "start"); code != 0 {
		t.Fatalf("start = %d with non-loopback listen (%s)", code, out)
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("something listens on the rejected tcp_listen port")
	}
	if _, err := os.Stat(home + "/token"); !os.IsNotExist(err) {
		t.Fatal("token file written for a rejected listener")
	}
}
