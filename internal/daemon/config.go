package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/search-3d/breakroom/internal/paths"
)

// Config is the daemon's resolved configuration.
type Config struct {
	WaitHoldMaxSec     int            `json:"wait_hold_max_sec"`
	DiffMaxBytes       int            `json:"diff_max_bytes"`
	SummarizeMaxBytes  int            `json:"summarize_max_bytes"`
	EndpointURL        string         `json:"endpoint_url"`
	APIKeyEnv          string         `json:"api_key_env"`
	Model              string         `json:"model"`
	ReasoningEffort    string         `json:"reasoning_effort"`
	ExtraBody          map[string]any `json:"extra_body"`
	BlueprintMaxRounds int            `json:"blueprint_max_rounds"`
	TCPListen          string         `json:"tcp_listen"`
	RetentionDays      int            `json:"retention_days"` // global only; < 0 keeps history forever
}

// maxBlueprintRounds caps blueprint_max_rounds from any config file.
const maxBlueprintRounds = 20

// defaultConfig is the configuration used when no config file sets a value.
func defaultConfig() Config {
	return Config{WaitHoldMaxSec: 45, DiffMaxBytes: 65536, SummarizeMaxBytes: 16384, BlueprintMaxRounds: 5, RetentionDays: 14}
}

// loadConfig overlays ~/.breakroom/config.json onto defaults. A missing file
// means all defaults; an unparseable file is an error.
func loadConfig() (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(paths.Config())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	var file Config
	if err := json.Unmarshal(data, &file); err != nil {
		return c, fmt.Errorf("breakroom: bad config %s: %w", paths.Config(), err)
	}
	c.overlay(file)
	if file.TCPListen != "" {
		c.TCPListen = file.TCPListen
	}
	if file.RetentionDays != 0 {
		c.RetentionDays = file.RetentionDays
	}
	return c, nil
}

// loadConfigFor reads the global config and lays the workspace's
// .breakroom/config.json over it. A project can't set global-only fields, and
// its endpoint must be on this machine.
func loadConfigFor(workspace string) (Config, error) {
	c, err := loadConfig()
	if err != nil || workspace == "" {
		return c, err
	}
	// A broken repo file (any clone can carry one) falls back to the global
	// config: it must not break get_diff, waits, or inspection.
	path := filepath.Join(workspace, ".breakroom", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("ignoring unreadable repo config", "path", path, "error", err)
		}
		return c, nil
	}
	var file Config
	if err := json.Unmarshal(data, &file); err != nil {
		slog.Warn("ignoring bad repo config", "path", path, "error", err)
		return c, nil
	}
	// Repo files are untrusted (any clone): they never pick which env var
	// becomes the bearer key, and the global key only goes to the global
	// endpoint. Projects can still point at keyless (local) endpoints.
	global, globalExtra, globalModel, globalEffort, globalHold := c.EndpointURL, c.ExtraBody, c.Model, c.ReasoningEffort, c.WaitHoldMaxSec
	file.APIKeyEnv = ""
	// A repo endpoint must be loopback: summarize/compact send uncommitted
	// diffs and workspace mail to it, so a remote one is an exfil channel
	// (and a link-local one reaches cloud metadata). Ignored, not an error:
	// a bad repo file must not break get_diff.
	if file.EndpointURL != "" && !loopbackURL(file.EndpointURL) {
		slog.Warn("ignoring non-loopback endpoint_url in repo config", "path", path, "endpoint_url", file.EndpointURL)
		file.EndpointURL = ""
	}
	c.overlay(file)
	if c.EndpointURL != global {
		c.APIKeyEnv = ""
	}
	// Nothing billable rides along with the global key: a project's
	// extra_body (n, max_completion_tokens), model, or reasoning_effort
	// would spend on the user's account.
	if c.APIKeyEnv != "" {
		c.ExtraBody, c.Model, c.ReasoningEffort = globalExtra, globalModel, globalEffort
	}
	// A project may shorten waits, never lengthen them past the host tool
	// timeouts the user tuned the global value for.
	c.WaitHoldMaxSec = min(c.WaitHoldMaxSec, globalHold)
	return c, nil
}

// overlay applies nonzero workspace-scoped fields with their clamps.
func (c *Config) overlay(file Config) {
	if file.WaitHoldMaxSec > 0 {
		c.WaitHoldMaxSec = min(file.WaitHoldMaxSec, 300)
	}
	if file.DiffMaxBytes > 0 {
		c.DiffMaxBytes = min(max(file.DiffMaxBytes, 1024), 16<<20)
	}
	if file.SummarizeMaxBytes > 0 {
		c.SummarizeMaxBytes = min(max(file.SummarizeMaxBytes, 1024), 1<<20)
	}
	if file.BlueprintMaxRounds > 0 {
		// Capped so a repo file cannot turn the stuck-loop brake off.
		c.BlueprintMaxRounds = min(file.BlueprintMaxRounds, maxBlueprintRounds)
	}
	if file.EndpointURL != "" {
		c.EndpointURL = file.EndpointURL
	}
	if file.APIKeyEnv != "" {
		c.APIKeyEnv = file.APIKeyEnv
	}
	if file.Model != "" {
		c.Model = file.Model
	}
	if file.ReasoningEffort != "" {
		c.ReasoningEffort = file.ReasoningEffort
	}
	if file.ExtraBody != nil {
		c.ExtraBody = file.ExtraBody
	}
}

// loopbackURL reports whether raw is a URL whose host is localhost or a
// loopback IP.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
