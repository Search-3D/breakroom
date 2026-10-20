package hosts_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/search-3d/breakroom/internal/hosts"
)

func TestHostsRegistry(t *testing.T) {
	expectedHosts := []string{"claude", "antigravity", "cursor", "codex", "vscode", "copilot", "muse", "opencode", "generic"}
	for _, h := range expectedHosts {
		target := hosts.Find(h)
		if target == nil {
			t.Errorf("expected host %q to be in hosts.Registry", h)
			continue
		}
		if target.ID != h {
			t.Errorf("target.ID = %q, want %q", target.ID, h)
		}
	}

	if hosts.Find("unknown-host") != nil {
		t.Error("expected Find(unknown-host) to return nil")
	}

	installTargets := hosts.InstallTargets()
	if len(installTargets) != 8 {
		t.Errorf("len(InstallTargets) = %d, want 8", len(installTargets))
	}

	if s := hosts.InstallHostsString(); s != "claude, antigravity, cursor, codex, vscode, muse, opencode, generic" {
		t.Errorf("InstallHostsString = %q", s)
	}
	if s := hosts.SupportedHostsString(); s != "claude, antigravity, cursor, codex, vscode, copilot, muse, opencode, generic" {
		t.Errorf("SupportedHostsString = %q", s)
	}
}

func TestFormatStopPayload(t *testing.T) {
	claude := hosts.Find("claude")
	claudePayload := claude.FormatStopPayload("stop reason")
	claudeData, _ := json.Marshal(claudePayload)
	expectedClaude := `{"decision":"block","reason":"stop reason"}`
	var claudeMap, expClaudeMap map[string]string
	_ = json.Unmarshal(claudeData, &claudeMap)
	_ = json.Unmarshal([]byte(expectedClaude), &expClaudeMap)
	if !reflect.DeepEqual(claudeMap, expClaudeMap) {
		t.Errorf("claude payload mismatch: got %s, want %s", string(claudeData), expectedClaude)
	}

	antigravity := hosts.Find("antigravity")
	agPayload := antigravity.FormatStopPayload("stop reason")
	agData, _ := json.Marshal(agPayload)
	expectedAG := `{"decision":"continue","reason":"stop reason"}`
	var agMap, expAGMap map[string]string
	_ = json.Unmarshal(agData, &agMap)
	_ = json.Unmarshal([]byte(expectedAG), &expAGMap)
	if !reflect.DeepEqual(agMap, expAGMap) {
		t.Errorf("antigravity payload mismatch: got %s, want %s", string(agData), expectedAG)
	}

	vscode := hosts.Find("vscode")
	vsPayload := vscode.FormatStopPayload("stop reason")
	vsData, _ := json.Marshal(vsPayload)
	var vsMap map[string]any
	_ = json.Unmarshal(vsData, &vsMap)
	nested, ok := vsMap["hookSpecificOutput"].(map[string]any)
	if !ok || nested["hookEventName"] != "Stop" || nested["decision"] != "block" || nested["reason"] != "stop reason" {
		t.Errorf("vscode payload mismatch: got %s", string(vsData))
	}
}
