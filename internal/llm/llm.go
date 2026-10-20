// Package llm calls an optional OpenAI-compatible chat endpoint for
// compaction. It is never in the critical path of message delivery.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/search-3d/breakroom/internal/render"
)

// ErrNotConfigured means endpoint_url/model are missing from the config.
var ErrNotConfigured = errors.New("llm endpoint not configured (set endpoint_url and model in config.json)")

const (
	// DefaultSummarizeMaxBytes caps diff text sent for summarization.
	DefaultSummarizeMaxBytes = 16384
	// MaxThreadMessageBytes caps each message fed to thread compaction.
	MaxThreadMessageBytes = 2000
	// MaxThreadBytes caps a whole thread digest input.
	MaxThreadBytes = 48 << 10
	// MaxTokens caps completion length (cost control).
	MaxTokens   = 1000
	callTimeout = 120 * time.Second
)

// Config describes the chat endpoint. APIKey may be empty (local endpoints
// without auth). EndpointURL is the full chat-completions URL.
type Config struct {
	EndpointURL       string
	APIKey            string
	Model             string
	SummarizeMaxBytes int
	// ReasoningEffort is sent as reasoning_effort when set; "none" turns
	// thinking off on Ollama and OpenAI-style reasoning models.
	ReasoningEffort string
	// ExtraBody is merged into the request for server-specific fields, e.g.
	// vLLM's chat_template_kwargs. It never overrides the fields chat sets.
	ExtraBody map[string]any
}

// NormalizeEndpointURL turns a base URL (like http://localhost:11434/v1) into
// the full chat completions URL. Full URLs are kept; a URL with no path gets
// /v1/chat/completions.
func NormalizeEndpointURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return raw
	}
	p := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(p, "/chat/completions"):
		u.Path = p
	case strings.HasSuffix(p, "/chat"):
		u.Path = p + "/completions"
	case p == "":
		u.Path = "/v1/chat/completions"
	default:
		u.Path = p + "/chat/completions"
	}
	return u.String()
}

// chatURL returns the normalized chat completions endpoint URL.
func (c Config) chatURL() string {
	return NormalizeEndpointURL(c.EndpointURL)
}

// tokenCapField names the completion cap field. OpenAI's reasoning models
// reject max_tokens, but Ollama (0.40) silently ignores
// max_completion_tokens, so only OpenAI and Azure OpenAI get the new name.
func (c Config) tokenCapField() string {
	u, err := url.Parse(c.chatURL())
	if err == nil {
		h := strings.ToLower(u.Hostname())
		if h == "api.openai.com" || strings.HasSuffix(h, ".openai.azure.com") {
			return "max_completion_tokens"
		}
	}
	return "max_tokens"
}

// Valid reports whether the endpoint is usable.
func (c Config) Valid() bool {
	if c.Model == "" {
		return false
	}
	u, err := url.Parse(c.chatURL())
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// summarizeCap is the most bytes sent for summarizing, with the default applied.
func (c Config) summarizeCap() int {
	if c.SummarizeMaxBytes > 0 {
		return c.SummarizeMaxBytes
	}
	return DefaultSummarizeMaxBytes
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// httpClient never follows redirects: a redirect would re-send the diff
// and mail in the request body to wherever it points.
var httpClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// chat posts one completion and returns the text.
func chat(ctx context.Context, cfg Config, system, user string) (string, error) {
	fields := map[string]any{}
	for k, v := range cfg.ExtraBody {
		fields[k] = v
	}
	delete(fields, "max_tokens")
	delete(fields, "max_completion_tokens")
	fields["model"] = cfg.Model
	fields["messages"] = []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	fields["temperature"] = 0.2
	fields[cfg.tokenCapField()] = MaxTokens
	if cfg.ReasoningEffort != "" {
		fields["reasoning_effort"] = cfg.ReasoningEffort
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.chatURL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: status %s: %s", resp.Status, snippet(string(data)))
	}
	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("llm: bad response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("llm: empty choices")
	}
	// Thinking models can spend the whole token budget reasoning and
	// return no content; say so rather than hand back a blank summary.
	if c := out.Choices[0]; strings.TrimSpace(c.Message.Content) == "" {
		return "", fmt.Errorf("llm: empty reply (finish_reason %q); a thinking model may have spent its token cap reasoning - set reasoning_effort to \"none\"", c.FinishReason)
	}
	return out.Choices[0].Message.Content, nil
}

// snippet trims an error body to at most 500 bytes for an error message.
func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = render.CutRunes(s, 500) + "..."
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// SummarizeDiff condenses a unified diff into a file-by-file brief.
func SummarizeDiff(ctx context.Context, cfg Config, files []string, diff string) (string, error) {
	const system = "You summarize code diffs. Reply with a file-by-file change list: " +
		"one line per file (path - what changed), then 1-3 sentences on overall intent. " +
		"No code blocks unless a 1-2 line snippet is essential. Keep it under 250 words."
	var b strings.Builder
	b.WriteString("Files:\n")
	for _, f := range files {
		b.WriteString("- ")   // returns 2
		b.WriteString(f)      // returns len(f)
		_ = b.WriteByte('\n') // returns error (nil)
	}
	b.WriteString("\nDiff (may be truncated):\n")
	text := diff
	if len(text) > cfg.summarizeCap() {
		cut := strings.LastIndex(text[:cfg.summarizeCap()], "\n")
		if cut < 0 {
			cut = cfg.summarizeCap()
		}
		text = text[:cut] + "\n[diff truncated for summary]"
	}
	b.WriteString(text)
	return chat(ctx, cfg, system, b.String())
}

// ThreadMessage is one message fed to compaction.
type ThreadMessage struct {
	From    string
	To      string
	Type    string
	Content string
}

// CompactThread condenses recent messages into a handoff brief.
func CompactThread(ctx context.Context, cfg Config, msgs []ThreadMessage) (string, error) {
	const system = "You write handoff briefs for coding agents joining mid-session. Reply with: " +
		"1) What was decided (bullets), 2) What each agent is doing (agent: task), " +
		"3) Open questions and blockers. Under 300 words. No preamble."
	return chat(ctx, cfg, system, Transcript(msgs))
}

// Transcript renders msgs as the capped, oldest-first text compaction
// reads. With no endpoint configured it is returned as-is so the calling
// agent can write the brief itself.
func Transcript(msgs []ThreadMessage) string {
	// Newest-first within budget, then back to chronological.
	var lines []string
	used := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		content := m.Content
		if len(content) > MaxThreadMessageBytes {
			content = render.CutRunes(content, MaxThreadMessageBytes) + "..."
		}
		line := "[" + m.From + "->" + m.To + "] (" + m.Type + "): " + content
		if used+len(line) > MaxThreadBytes && len(lines) > 0 {
			break
		}
		lines = append(lines, line)
		used += len(line)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return "Messages (oldest first):\n" + strings.Join(lines, "\n")
}
