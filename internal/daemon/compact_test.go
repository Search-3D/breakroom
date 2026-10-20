package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubLLM serves canned completions and records the last request body.
func stubLLM(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		*seen = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"DIGEST"}}]}`))
	}))
}

// writeLLMConfig points a temp BREAKROOM_HOME at an LLM endpoint.
func writeLLMConfig(t *testing.T, url string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	cfg := `{"endpoint_url": "` + url + `", "model": "test"}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompactEndpoint(t *testing.T) {
	var seen string
	srv := stubLLM(t, &seen)
	defer srv.Close()
	writeLLMConfig(t, srv.URL)
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws/wt-a", "backend")
	a2 := register(t, h, "/ws", "/ws/wt-b", "frontend")
	doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a1, "to": a2, "content": "decided X",
	})
	doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a2, "to": a1, "content": "doing Y",
	})
	code, out := doPost(t, h, "/v1/messages/compact", map[string]any{"workspace_root": "/ws"})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["digest"] != "DIGEST" || out["message_count"] != float64(2) {
		t.Fatalf("out = %v", out)
	}
	if !strings.Contains(seen, "decided X") || !strings.Contains(seen, "doing Y") {
		t.Fatalf("llm input = %q", seen)
	}
}

func TestCompactUnconfigured(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	h := newTestServer(t).Handler()
	a1 := register(t, h, "/ws", "/ws", "backend")
	a2 := register(t, h, "/ws", "/ws/wt-b", "frontend")
	doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": "/ws", "from": a1, "to": a2, "content": "hi",
	})
	code, out := doPost(t, h, "/v1/messages/compact", map[string]any{"workspace_root": "/ws"})
	if code != 200 || out["fallback"] != true || out["message_count"] != float64(1) {
		t.Fatalf("code=%d out=%v, want fallback transcript", code, out)
	}
	if d, _ := out["digest"].(string); !strings.Contains(d, "hi") {
		t.Fatalf("digest = %q, want raw thread", d)
	}

	// Empty inbox short-circuits before touching the LLM.
	code, out = doPost(t, h, "/v1/messages/compact", map[string]any{"workspace_root": "/empty"})
	if code != 200 || out["digest"] != "" || out["message_count"] != float64(0) {
		t.Fatalf("code=%d out=%v, want empty digest", code, out)
	}
}

func TestDiffSummarize(t *testing.T) {
	var seen string
	srv := stubLLM(t, &seen)
	defer srv.Close()
	writeLLMConfig(t, srv.URL)
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", "a\nb\n")
	sh(t, repo, "add", "-A")
	sh(t, repo, "commit", "-m", "init")
	writeFile(t, repo, "f.txt", "a\nB\n")
	h := newTestServer(t).Handler()
	a1 := register(t, h, repo, repo, "backend")

	code, out := doPost(t, h, "/v1/git/diff", map[string]any{
		"workspace_root": repo, "agent_id": a1, "summarize": true,
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["summary"] != "DIGEST" {
		t.Fatalf("summary = %v", out["summary"])
	}
	if diff, _ := out["diff"].(string); !strings.Contains(diff, "-b\n+B") {
		t.Fatalf("diff = %q", diff)
	}
	if !strings.Contains(seen, "f.txt") {
		t.Fatalf("llm input = %q", seen)
	}
}

func TestDiffSummarizeUnconfigured(t *testing.T) {
	t.Setenv("BREAKROOM_HOME", t.TempDir())
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", "a\n")
	sh(t, repo, "add", "-A")
	sh(t, repo, "commit", "-m", "init")
	writeFile(t, repo, "f.txt", "a\nb\n")
	h := newTestServer(t).Handler()
	a1 := register(t, h, repo, repo, "backend")
	code, out := doPost(t, h, "/v1/git/diff", map[string]any{
		"workspace_root": repo, "agent_id": a1, "summarize": true,
	})
	if code != 200 || out["fallback"] != true || out["summary"] != "" {
		t.Fatalf("code=%d out=%v, want fallback", code, out)
	}
	if d, _ := out["diff"].(string); !strings.Contains(d, "+b") {
		t.Fatalf("diff = %q", d)
	}
}

func TestSendDiffAttach(t *testing.T) {
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", "a\nb\n")
	writeFile(t, repo, "other.txt", "x\n")
	sh(t, repo, "add", "-A")
	sh(t, repo, "commit", "-m", "init")
	writeFile(t, repo, "f.txt", "a\nB\n")
	writeFile(t, repo, "other.txt", "x\nY\n")
	h := newTestServer(t).Handler()
	a1 := register(t, h, repo, repo, "backend")
	a2 := register(t, h, repo, repo, "frontend")

	code, out := doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": repo, "from": a1, "to": a2,
		"content": "review", "include_diff_paths": []string{"f.txt"},
	})
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	code, out = doPost(t, h, "/v1/messages/take", map[string]any{
		"workspace_root": repo, "recipient": a2,
	})
	if code != 200 {
		t.Fatalf("take code=%d", code)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	att, _ := msgs[0].(map[string]any)["diff_attachment"].(string)
	if !strings.Contains(att, "-b\n+B") || strings.Contains(att, "other.txt") {
		t.Fatalf("attachment = %q, want f.txt hunk only", att)
	}

	code, out = doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": repo, "from": "agent-9", "to": a2, "content": "x",
	})
	wantErr(t, code, out, "unknown_agent")
}

// A configured sidecar that fails is llm_error, never a silent fallback;
// an unreadable config is bad_request.
func TestCompactSidecarFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	writeLLMConfig(t, srv.URL)
	repo := initRepo(t)
	writeFile(t, repo, "f.txt", "a\n")
	sh(t, repo, "add", "-A")
	sh(t, repo, "commit", "-m", "init")
	writeFile(t, repo, "f.txt", "a\nb\n")
	h := newTestServer(t).Handler()
	a1 := register(t, h, repo, repo, "backend")
	a2 := register(t, h, repo, repo, "frontend")
	doPost(t, h, "/v1/messages/send", map[string]any{
		"workspace_root": repo, "from": a1, "to": a2, "content": "hi",
	})
	code, out := doPost(t, h, "/v1/messages/compact", map[string]any{"workspace_root": repo})
	wantErr(t, code, out, "llm_error")
	code, out = doPost(t, h, "/v1/git/diff", map[string]any{
		"workspace_root": repo, "agent_id": a1, "summarize": true,
	})
	wantErr(t, code, out, "llm_error")

	if err := os.MkdirAll(filepath.Join(repo, ".breakroom"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".breakroom", "config.json"), []byte(`{bad`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A malformed repo file falls back to the global config, so the same
	// endpoint failure comes back rather than a config error.
	code, out = doPost(t, h, "/v1/messages/compact", map[string]any{"workspace_root": repo})
	wantErr(t, code, out, "llm_error")
}
