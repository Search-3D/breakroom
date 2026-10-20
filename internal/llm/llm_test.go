package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// stub serves a fake chat endpoint that returns body with status, recording
// the request body and Authorization header it received.
func stub(t *testing.T, body string, status int, seen *map[string]any, auth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request: %v", err)
		}
		*seen = req
		*auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

const canned = `{"choices":[{"message":{"content":"DIGEST"}}]}`

func TestSummarizeDiff(t *testing.T) {
	var seen map[string]any
	var auth string
	srv := stub(t, canned, 200, &seen, &auth)
	defer srv.Close()
	cfg := Config{EndpointURL: srv.URL, APIKey: "k", Model: "m"}
	out, err := SummarizeDiff(context.Background(), cfg, []string{"a.go"}, "diff text")
	if err != nil {
		t.Fatal(err)
	}
	if out != "DIGEST" {
		t.Fatalf("out = %q", out)
	}
	if seen["model"] != "m" || seen["temperature"] != 0.2 || seen["max_tokens"] != float64(1000) || seen["max_completion_tokens"] != nil {
		t.Fatalf("req = %v", seen)
	}
	msgs, _ := seen["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	user, _ := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "a.go") || !strings.Contains(user, "diff text") {
		t.Fatalf("user = %q", user)
	}
	if auth != "Bearer k" {
		t.Fatalf("auth = %q", auth)
	}
}

func TestSummarizeNoKeyNoHeader(t *testing.T) {
	var seen map[string]any
	var auth string
	srv := stub(t, canned, 200, &seen, &auth)
	defer srv.Close()
	cfg := Config{EndpointURL: srv.URL, Model: "m"}
	if _, err := SummarizeDiff(context.Background(), cfg, nil, "d"); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Fatalf("auth = %q, want empty", auth)
	}
	if _, ok := seen["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort sent unset: %v", seen)
	}
	cfg.ReasoningEffort = "none"
	if _, err := SummarizeDiff(context.Background(), cfg, nil, "d"); err != nil {
		t.Fatal(err)
	}
	if seen["reasoning_effort"] != "none" {
		t.Fatalf("req = %v, want reasoning_effort none", seen)
	}
	// Extras merge in, but never override the fields chat sets.
	cfg.ExtraBody = map[string]any{
		"chat_template_kwargs":  map[string]any{"enable_thinking": false},
		"max_tokens":            99999,
		"max_completion_tokens": 99999,
		"model":                 "evil",
	}
	if _, err := SummarizeDiff(context.Background(), cfg, nil, "d"); err != nil {
		t.Fatal(err)
	}
	kw, _ := seen["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false || seen["max_tokens"] != float64(1000) || seen["max_completion_tokens"] != nil || seen["model"] != "m" {
		t.Fatalf("req = %v, want extras merged under core fields", seen)
	}
}

func TestTokenCapField(t *testing.T) {
	for url, want := range map[string]string{
		"https://api.openai.com/v1":                       "max_completion_tokens",
		"https://x.openai.azure.com/openai/deployments/d": "max_completion_tokens",
		"http://localhost:11434/v1/chat/completions":      "max_tokens",
		"https://api.groq.com/openai/v1/chat/completions": "max_tokens",
		"https://openrouter.ai/api/v1/chat/completions":   "max_tokens",
	} {
		if got := (Config{EndpointURL: url}).tokenCapField(); got != want {
			t.Errorf("%s: got %s, want %s", url, got, want)
		}
	}
}

func TestSummarizeTruncates(t *testing.T) {
	var seen map[string]any
	var auth string
	srv := stub(t, canned, 200, &seen, &auth)
	defer srv.Close()
	cfg := Config{EndpointURL: srv.URL, Model: "m", SummarizeMaxBytes: 100}
	big := "x\n" + strings.Repeat("0123456789\n", 50)
	if _, err := SummarizeDiff(context.Background(), cfg, []string{"f"}, big); err != nil {
		t.Fatal(err)
	}
	msgs, _ := seen["messages"].([]any)
	user, _ := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "[diff truncated for summary]") {
		t.Fatalf("user not truncated: %q", user)
	}
	if strings.Count(user, "0123456789") > 12 {
		t.Fatalf("user carries too much diff")
	}
}

func TestCompactThread(t *testing.T) {
	var seen map[string]any
	var auth string
	srv := stub(t, canned, 200, &seen, &auth)
	defer srv.Close()
	cfg := Config{EndpointURL: srv.URL, Model: "m"}
	msgs := []ThreadMessage{
		{From: "agent-1", To: "agent-2", Type: "chat", Content: "first"},
		{From: "agent-2", To: "agent-1", Type: "handoff", Content: strings.Repeat("y", 3000)},
	}
	out, err := CompactThread(context.Background(), cfg, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if out != "DIGEST" {
		t.Fatalf("out = %q", out)
	}
	got, _ := seen["messages"].([]any)
	user, _ := got[1].(map[string]any)["content"].(string)
	first, second := strings.Index(user, "first"), strings.Index(user, "agent-2->agent-1")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("order wrong: %q", user)
	}
	if strings.Count(user, "y") > MaxThreadMessageBytes+10 {
		t.Fatalf("per-message cap not applied")
	}
}

func TestCompactThreadBudget(t *testing.T) {
	var seen map[string]any
	var auth string
	srv := stub(t, canned, 200, &seen, &auth)
	defer srv.Close()
	cfg := Config{EndpointURL: srv.URL, Model: "m"}
	var msgs []ThreadMessage
	for i := 0; i < 100; i++ {
		msgs = append(msgs, ThreadMessage{From: "a", To: "b", Type: "chat", Content: strings.Repeat("z", 1000)})
	}
	if _, err := CompactThread(context.Background(), cfg, msgs); err != nil {
		t.Fatal(err)
	}
	got, _ := seen["messages"].([]any)
	user, _ := got[1].(map[string]any)["content"].(string)
	if len(user) > MaxThreadBytes+3000 {
		t.Fatalf("thread budget exceeded: %d bytes", len(user))
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"status 500", `{"error":"nope"}`, 500, "500"},
		{"bad json", `{bad`, 200, "bad response"},
		{"empty choices", `{"choices":[]}`, 200, "empty choices"},
		{"reasoning only", `{"choices":[{"message":{"content":"","reasoning":"hmm"},"finish_reason":"length"}]}`, 200, "reasoning_effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen map[string]any
			var auth string
			srv := stub(t, tc.body, tc.status, &seen, &auth)
			defer srv.Close()
			cfg := Config{EndpointURL: srv.URL, Model: "m"}
			_, err := SummarizeDiff(context.Background(), cfg, nil, "d")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{"http", Config{EndpointURL: "http://x/v1/chat", Model: "m"}, true},
		{"empty model", Config{EndpointURL: "http://x"}, false},
		{"empty url", Config{Model: "m"}, false},
		{"bad scheme", Config{EndpointURL: "ftp://x", Model: "m"}, false},
		{"malformed", Config{EndpointURL: "://bad", Model: "m"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Valid(); got != tc.want {
				t.Fatalf("%+v valid = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

func TestNormalizeEndpointURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"groq base url", "https://api.groq.com/openai/v1", "https://api.groq.com/openai/v1/chat/completions"},
		{"groq base url trailing slash", "https://api.groq.com/openai/v1/", "https://api.groq.com/openai/v1/chat/completions"},
		{"groq full completions", "https://api.groq.com/openai/v1/chat/completions", "https://api.groq.com/openai/v1/chat/completions"},
		{"groq full completions trailing slash", "https://api.groq.com/openai/v1/chat/completions/", "https://api.groq.com/openai/v1/chat/completions"},
		{"openai base url", "https://api.openai.com/v1", "https://api.openai.com/v1/chat/completions"},
		{"openai full completions", "https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"ollama base url with v1", "http://localhost:11434/v1", "http://localhost:11434/v1/chat/completions"},
		{"ollama base url with v1 trailing slash", "http://localhost:11434/v1/", "http://localhost:11434/v1/chat/completions"},
		{"ollama bare host", "http://localhost:11434", "http://localhost:11434/v1/chat/completions"},
		{"ollama bare host trailing slash", "http://localhost:11434/", "http://localhost:11434/v1/chat/completions"},
		{"openrouter base url", "https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/chat/completions"},
		{"custom chat path", "http://x/v1/chat", "http://x/v1/chat/completions"},
		{"custom chat path trailing slash", "http://x/v1/chat/", "http://x/v1/chat/completions"},
		{"azure deployment query", "https://my-resource.openai.azure.com/openai/deployments/my-gpt4?api-version=2024-02-15-preview", "https://my-resource.openai.azure.com/openai/deployments/my-gpt4/chat/completions?api-version=2024-02-15-preview"},
		{"empty string", "", ""},
		{"bad scheme", "ftp://example.com/v1", "ftp://example.com/v1"},
		{"malformed", "://bad", "://bad"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeEndpointURL(tc.in)
			if got != tc.want {
				t.Errorf("NormalizeEndpointURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Test idempotence
			if got2 := NormalizeEndpointURL(got); got2 != tc.want {
				t.Errorf("NormalizeEndpointURL(NormalizeEndpointURL(%q)) = %q, want %q", tc.in, got2, tc.want)
			}
		})
	}
}

// Cuts land on rune boundaries: a split character would send invalid UTF-8.
func TestCutsKeepUTF8(t *testing.T) {
	long := strings.Repeat("é", MaxThreadMessageBytes) // 2 bytes each
	if got := Transcript([]ThreadMessage{{From: "a", To: "b", Type: "chat", Content: "x" + long}}); !utf8.ValidString(got) {
		t.Fatal("Transcript split a rune")
	}
	if got := snippet("x" + strings.Repeat("é", 500)); !utf8.ValidString(got) {
		t.Fatal("snippet split a rune")
	}
}
