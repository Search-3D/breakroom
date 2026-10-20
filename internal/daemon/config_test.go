package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WaitHoldMaxSec != 45 {
		t.Fatalf("default hold = %d, want 45", cfg.WaitHoldMaxSec)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"wait_hold_max_sec": 10}`)
	cfg, err = loadConfig()
	if err != nil || cfg.WaitHoldMaxSec != 10 {
		t.Fatalf("cfg = %+v, %v; want hold 10", cfg, err)
	}
	write(`{"wait_hold_max_sec": 9999}`)
	cfg, err = loadConfig()
	if err != nil || cfg.WaitHoldMaxSec != 300 {
		t.Fatalf("cfg = %+v, %v; want hold clamped to 300", cfg, err)
	}
	write(`{"diff_max_bytes": 1024}`)
	cfg, err = loadConfig()
	if err != nil || cfg.DiffMaxBytes != 1024 {
		t.Fatalf("cfg = %+v, %v; want diff cap 1024", cfg, err)
	}
	write(`{"endpoint_url": "http://x/v1/chat", "api_key_env": "K", "model": "m", "summarize_max_bytes": 2048, "reasoning_effort": "none"}`)
	cfg, err = loadConfig()
	if err != nil || cfg.EndpointURL != "http://x/v1/chat" || cfg.APIKeyEnv != "K" ||
		cfg.Model != "m" || cfg.SummarizeMaxBytes != 2048 || cfg.ReasoningEffort != "none" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	write(`{"tcp_listen": "127.0.0.1:7420"}`)
	cfg, err = loadConfig()
	if err != nil || cfg.TCPListen != "127.0.0.1:7420" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	write(`{bad`)
	if _, err := loadConfig(); err == nil {
		t.Fatal("bad JSON: want error, got nil")
	}
}

func TestLoadConfigFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"),
		[]byte(`{"wait_hold_max_sec": 10, "model": "global", "tcp_listen": "127.0.0.1:1"}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	// No project file: global stands.
	cfg, err := loadConfigFor(ws)
	if err != nil || cfg.WaitHoldMaxSec != 10 || cfg.Model != "global" {
		t.Fatalf("cfg = %+v, %v; want global", cfg, err)
	}
	// Project file overlays workspace fields, never tcp_listen; it may
	// shorten the wait hold but not lengthen it.
	if err := os.MkdirAll(filepath.Join(ws, ".breakroom"), 0o700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(ws, ".breakroom", "config.json")
	if err := os.WriteFile(project,
		[]byte(`{"wait_hold_max_sec": 5, "model": "local", "tcp_listen": "127.0.0.1:2"}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfigFor(ws)
	if err != nil || cfg.WaitHoldMaxSec != 5 || cfg.Model != "local" {
		t.Fatalf("cfg = %+v, %v; want project overlay", cfg, err)
	}
	if cfg.TCPListen != "127.0.0.1:1" {
		t.Fatalf("tcp_listen = %q, want global :1", cfg.TCPListen)
	}
	if err := os.WriteFile(project, []byte(`{"wait_hold_max_sec": 300}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadConfigFor(ws); err != nil || cfg.WaitHoldMaxSec != 10 {
		t.Fatalf("hold = %d, %v; want global 10", cfg.WaitHoldMaxSec, err)
	}
	// A project file never chooses which env var becomes the bearer key,
	// moving the endpoint drops the global key (it only goes to the global
	// endpoint), and a non-loopback project endpoint is ignored: a cloned
	// repo cannot exfiltrate daemon env vars, diffs, or mail.
	if err := os.WriteFile(filepath.Join(home, "config.json"),
		[]byte(`{"wait_hold_max_sec": 10, "endpoint_url": "http://global/v1", "api_key_env": "GLOBAL_KEY", "model": "m"}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ body, wantURL, wantKey string }{
		{`{"api_key_env": "GITHUB_TOKEN"}`, "http://global/v1", "GLOBAL_KEY"},
		{`{"endpoint_url": "http://evil/v1", "api_key_env": "GITHUB_TOKEN"}`, "http://global/v1", "GLOBAL_KEY"},
		{`{"endpoint_url": "http://evil/v1"}`, "http://global/v1", "GLOBAL_KEY"},
		{`{"endpoint_url": "http://169.254.169.254/v1"}`, "http://global/v1", "GLOBAL_KEY"},
		{`{"endpoint_url": "http://127.0.0.1.evil.com/v1"}`, "http://global/v1", "GLOBAL_KEY"},
		{`{"endpoint_url": "http://localhost:11434/v1"}`, "http://localhost:11434/v1", ""},
		{`{"endpoint_url": "http://127.0.0.1:11434/v1", "api_key_env": "GITHUB_TOKEN"}`, "http://127.0.0.1:11434/v1", ""},
		{`{"endpoint_url": "http://[::1]:8000/v1"}`, "http://[::1]:8000/v1", ""},
		{`{"endpoint_url": "http://global/v1", "model": "local"}`, "http://global/v1", "GLOBAL_KEY"},
	} {
		if err := os.WriteFile(project, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err = loadConfigFor(ws)
		if err != nil || cfg.EndpointURL != tc.wantURL || cfg.APIKeyEnv != tc.wantKey {
			t.Fatalf("%s: cfg = %+v, %v; want url %q key %q", tc.body, cfg, err, tc.wantURL, tc.wantKey)
		}
	}
	// With the global key sent, a project can't pick the model or effort
	// it's billed for; against its own local endpoint it can.
	for _, tc := range []struct{ body, wantModel, wantEffort string }{
		{`{"model": "o1-pro", "reasoning_effort": "high"}`, "m", ""},
		{`{"endpoint_url": "http://localhost/v1", "model": "gemma", "reasoning_effort": "none"}`, "gemma", "none"},
	} {
		if err := os.WriteFile(project, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err = loadConfigFor(ws)
		if err != nil || cfg.Model != tc.wantModel || cfg.ReasoningEffort != tc.wantEffort {
			t.Fatalf("%s: model %q effort %q, %v; want %q %q", tc.body, cfg.Model, cfg.ReasoningEffort, err, tc.wantModel, tc.wantEffort)
		}
	}
	// Project extra_body applies only when the global key isn't sent.
	for _, tc := range []struct {
		body      string
		wantExtra bool
	}{
		{`{"extra_body": {"n": 50}}`, false},
		{`{"endpoint_url": "http://localhost/v1", "extra_body": {"n": 50}}`, true},
	} {
		if err := os.WriteFile(project, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err = loadConfigFor(ws)
		if err != nil || (cfg.ExtraBody != nil) != tc.wantExtra {
			t.Fatalf("%s: extra = %v, %v; want present=%v", tc.body, cfg.ExtraBody, err, tc.wantExtra)
		}
	}
	// blueprint_max_rounds is capped so a repo cannot disable the brake.
	if err := os.WriteFile(project, []byte(`{"blueprint_max_rounds": 1000000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadConfigFor(ws); err != nil || cfg.BlueprintMaxRounds != maxBlueprintRounds {
		t.Fatalf("rounds = %d, %v; want %d", cfg.BlueprintMaxRounds, err, maxBlueprintRounds)
	}
	// Bad project JSON is ignored (global stands); empty workspace skips
	// the overlay.
	if err := os.WriteFile(project, []byte(`{bad}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = loadConfigFor(ws); err != nil || cfg.WaitHoldMaxSec != 10 {
		t.Fatalf("bad project JSON: cfg = %+v, %v; want global", cfg, err)
	}
	cfg, err = loadConfigFor("")
	if err != nil || cfg.WaitHoldMaxSec != 10 {
		t.Fatalf("cfg = %+v, %v; want global", cfg, err)
	}
}

// retention_days defaults to 14; negative keeps history forever.
func TestRetentionDays(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	if cfg, err := loadConfig(); err != nil || cfg.RetentionDays != 14 {
		t.Fatalf("default = %d, %v; want 14", cfg.RetentionDays, err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"retention_days": -1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(); err != nil || cfg.RetentionDays != -1 {
		t.Fatalf("set = %d, %v; want -1", cfg.RetentionDays, err)
	}
}

func TestResolveLLMNormalizeEndpointURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BREAKROOM_HOME", home)
	// Base URL for Groq in global config
	cfgJSON := `{"endpoint_url": "https://api.groq.com/openai/v1", "model": "llama-3.3-70b-versatile"}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	llmCfg, err := resolveLLM("")
	if err != nil {
		t.Fatalf("resolveLLM failed: %v", err)
	}
	wantURL := "https://api.groq.com/openai/v1/chat/completions"
	if llmCfg.EndpointURL != wantURL {
		t.Fatalf("llmCfg.EndpointURL = %q, want %q", llmCfg.EndpointURL, wantURL)
	}
}
