package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/search-3d/breakroom"
)

func TestInstallCommandWithHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		host     string
		wantCode int
		wantRel  string
	}{
		{"generic host", "generic", 0, filepath.Join(".agents", "skills", "breakroom")},
		{"invalid host", "invalid-host", 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			code := runInstall(context.Background(), []string{"--host", tc.host, tmp})
			if code != tc.wantCode {
				t.Fatalf("runInstall --host %s returned %d, want %d", tc.host, code, tc.wantCode)
			}
			if tc.wantRel == "" {
				return
			}
			target := filepath.Join(tmp, tc.wantRel)
			skillData, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
			if err != nil {
				t.Fatalf("reading installed SKILL.md: %v", err)
			}
			if !bytes.Equal(skillData, breakroom.SkillMD) {
				t.Errorf("installed SKILL.md does not match embedded SkillMD")
			}

			toolContractsData, err := os.ReadFile(filepath.Join(target, "docs", "tool-contracts.md"))
			if err != nil {
				t.Fatalf("reading installed tool-contracts.md: %v", err)
			}
			if !bytes.Equal(toolContractsData, breakroom.ToolContractsMD) {
				t.Errorf("installed tool-contracts.md does not match embedded ToolContractsMD")
			}

			blueprintSpecData, err := os.ReadFile(filepath.Join(target, "docs", "blueprint-spec.md"))
			if err != nil {
				t.Fatalf("reading installed blueprint-spec.md: %v", err)
			}
			if !bytes.Equal(blueprintSpecData, breakroom.BlueprintSpecMD) {
				t.Errorf("installed blueprint-spec.md does not match embedded BlueprintSpecMD")
			}
		})
	}
}

func TestInstallCommandDefaultDirWithHost(t *testing.T) {
	tmp := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting wd: %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		_ = os.Chdir(origWd)
	}()

	code := runInstall(context.Background(), []string{"--host", "generic"})
	if code != 0 {
		t.Fatalf("runInstall --host generic returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".agents", "skills", "breakroom")
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Fatalf("expected installed SKILL.md in default dir: %v", err)
	}
}

func TestInstallCommandAntigravity(t *testing.T) {
	tmp := t.TempDir()
	code := runInstall(context.Background(), []string{"--host", "antigravity", tmp})
	if code != 0 {
		t.Fatalf("runInstall --host antigravity returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".agents", "plugins", "breakroom")
	for _, check := range []struct {
		rel  string
		want []byte
	}{
		{"plugin.json", breakroom.AntigravityPluginJSON},
		{"mcp_config.json", breakroom.AntigravityMcpConfigJSON},
		{"hooks.json", breakroom.AntigravityHooksJSON},
		{filepath.Join("skills", "breakroom", "SKILL.md"), breakroom.SkillMD},
		{filepath.Join("skills", "breakroom", "docs", "tool-contracts.md"), breakroom.ToolContractsMD},
		{filepath.Join("skills", "breakroom", "docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
	} {
		data, err := os.ReadFile(filepath.Join(target, check.rel))
		if err != nil {
			t.Fatalf("reading installed %s: %v", check.rel, err)
		}
		if !bytes.Equal(data, check.want) {
			t.Errorf("installed %s does not match expected embedded data", check.rel)
		}
	}
}

func TestInstallCommandAntigravityDefaultGlobal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "antigravity"})
	if code != 0 {
		t.Fatalf("runInstall --host antigravity returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".gemini", "config", "plugins", "breakroom")
	for _, rel := range []string{"plugin.json", "mcp_config.json", "hooks.json", filepath.Join("skills", "breakroom", "SKILL.md")} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("expected global installed file %s: %v", rel, err)
		}
	}
}

func TestInstallCommandGlobalFlag(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "generic", "--global"})
	if code != 0 {
		t.Fatalf("runInstall --host generic --global returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".agents", "skills", "breakroom")
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Errorf("expected global installed SKILL.md for generic: %v", err)
	}
}

func TestInstallCommandCursor(t *testing.T) {
	tmp := t.TempDir()
	cursorDir := filepath.Join(tmp, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Existing project config must survive the install and the uninstall.
	if err := os.WriteFile(filepath.Join(cursorDir, "mcp.json"), []byte(`{"mcpServers":{"other":{"command":"other"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code := runInstall(context.Background(), []string{"--host", "cursor", tmp})
	if code != 0 {
		t.Fatalf("runInstall --host cursor returned %d, want 0", code)
	}

	mcp, err := os.ReadFile(filepath.Join(cursorDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"other"`, `"breakroom"`} {
		if !strings.Contains(string(mcp), want) {
			t.Errorf("mcp.json missing %s: %s", want, mcp)
		}
	}
	hooks, err := os.ReadFile(filepath.Join(cursorDir, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hooks), "breakroom gate --event stop --host cursor") {
		t.Errorf("hooks.json missing stop hook: %s", hooks)
	}
	if _, err := os.Stat(filepath.Join(cursorDir, "skills", "breakroom", "SKILL.md")); err != nil {
		t.Errorf("skill not installed: %v", err)
	}

	// Re-install must not duplicate hooks.
	if code := runInstall(context.Background(), []string{"--host", "cursor", tmp}); code != 0 {
		t.Fatalf("re-install returned %d", code)
	}
	hooks, _ = os.ReadFile(filepath.Join(cursorDir, "hooks.json"))
	if n := strings.Count(string(hooks), "--event stop"); n != 1 {
		t.Errorf("stop hook appears %d times after re-install, want 1", n)
	}

	t.Setenv("HOME", t.TempDir())
	if code := runUninstall(context.Background(), []string{"--host", "cursor", tmp}); code != 0 {
		t.Fatalf("uninstall returned %d", code)
	}
	mcp, _ = os.ReadFile(filepath.Join(cursorDir, "mcp.json"))
	if strings.Contains(string(mcp), "breakroom") || !strings.Contains(string(mcp), "other") {
		t.Errorf("uninstall should leave only the other server: %s", mcp)
	}
	for _, gone := range []string{"hooks.json", filepath.Join("skills", "breakroom")} {
		if _, err := os.Stat(filepath.Join(cursorDir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed after uninstall", gone)
		}
	}
}

func TestUninstallClaudeLocalSettings(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if code := runInstall(context.Background(), []string{"--host", "claude", tmp}); code != 0 {
		t.Fatalf("install returned %d", code)
	}
	local := filepath.Join(tmp, ".claude", "settings.local.json")
	if err := os.WriteFile(local, []byte(`{"enabledMcpjsonServers":["plugin:breakroom:breakroom"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runUninstall(context.Background(), []string{"--host", "claude", tmp}); code != 0 {
		t.Fatalf("uninstall returned %d", code)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Errorf("settings.local.json should be deleted when only Breakroom was in it: %v", err)
	}
}

func TestInstallCommandCursorDefaultGlobal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "cursor"})
	if code != 0 {
		t.Fatalf("runInstall --host cursor returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".cursor", "plugins", "local", "breakroom")
	for _, rel := range []string{
		"plugin.json",
		"mcp.json",
		filepath.Join("hooks", "hooks.json"),
		filepath.Join("skills", "breakroom", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("expected global installed file %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, ".cursor-plugin")); !os.IsNotExist(err) {
		t.Errorf(".cursor-plugin directory should not exist in installed plugin")
	}
}

func TestInstallCommandOpenCode(t *testing.T) {
	tmp := t.TempDir()
	code := runInstall(context.Background(), []string{"--host", "opencode", tmp})
	if code != 0 {
		t.Fatalf("runInstall --host opencode returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".opencode")
	for _, check := range []struct {
		rel  string
		want []byte
	}{
		{filepath.Join("plugins", "breakroom.ts"), breakroom.OpenCodePluginTS},
		{filepath.Join("skills", "breakroom", "SKILL.md"), breakroom.SkillMD},
		{filepath.Join("skills", "breakroom", "docs", "tool-contracts.md"), breakroom.ToolContractsMD},
		{filepath.Join("skills", "breakroom", "docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
	} {
		data, err := os.ReadFile(filepath.Join(target, check.rel))
		if err != nil {
			t.Fatalf("reading installed %s: %v", check.rel, err)
		}
		if !bytes.Equal(data, check.want) {
			t.Errorf("installed %s does not match expected embedded data", check.rel)
		}
	}
}

func TestInstallCommandOpenCodeGlobal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "opencode", "--global"})
	if code != 0 {
		t.Fatalf("runInstall --host opencode --global returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".config", "opencode")
	for _, check := range []struct {
		rel  string
		want []byte
	}{
		{filepath.Join("plugins", "breakroom.ts"), breakroom.OpenCodePluginTS},
		{filepath.Join("skills", "breakroom", "SKILL.md"), breakroom.SkillMD},
		{filepath.Join("skills", "breakroom", "docs", "tool-contracts.md"), breakroom.ToolContractsMD},
		{filepath.Join("skills", "breakroom", "docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
	} {
		data, err := os.ReadFile(filepath.Join(target, check.rel))
		if err != nil {
			t.Fatalf("reading installed %s: %v", check.rel, err)
		}
		if !bytes.Equal(data, check.want) {
			t.Errorf("installed %s does not match expected embedded data", check.rel)
		}
	}
}

func TestInstallCommandVSCode(t *testing.T) {
	tmp := t.TempDir()
	code := runInstall(context.Background(), []string{"--host", "vscode", tmp})
	if code != 0 {
		t.Fatalf("runInstall --host vscode returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".vscode", "plugins", "breakroom")
	for _, check := range []struct {
		rel  string
		want []byte
	}{
		{"plugin.json", breakroom.VSCodePluginJSON},
		{"mcp.json", breakroom.VSCodeMcpJSON},
		{filepath.Join("com.github.copilot", "hooks", "hooks.json"), breakroom.VSCodeHooksJSON},
		{filepath.Join("skills", "breakroom", "SKILL.md"), breakroom.SkillMD},
		{filepath.Join("skills", "breakroom", "docs", "tool-contracts.md"), breakroom.ToolContractsMD},
		{filepath.Join("skills", "breakroom", "docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
		{filepath.Join("..", "..", "..", ".github", "hooks", "breakroom-copilot.json"), breakroom.VSCodeCopilotHooksJSON},
	} {
		data, err := os.ReadFile(filepath.Join(target, check.rel))
		if err != nil {
			t.Fatalf("reading installed %s: %v", check.rel, err)
		}
		if !bytes.Equal(data, check.want) {
			t.Errorf("installed %s does not match expected embedded data", check.rel)
		}
	}
}

func TestInstallCommandVSCodeDefaultGlobal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "vscode"})
	if code != 0 {
		t.Fatalf("runInstall --host vscode returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".copilot", "installed-plugins", "_direct", "breakroom")
	for _, rel := range []string{
		"plugin.json",
		"mcp.json",
		filepath.Join("com.github.copilot", "hooks", "hooks.json"),
		filepath.Join("skills", "breakroom", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("expected global installed file %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fakeHome, ".copilot", "hooks", "breakroom-copilot.json")); err != nil {
		t.Errorf("expected global Copilot hooks: %v", err)
	}
}

func TestInstallCommandCopilotAlias(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "copilot"})
	if code != 0 {
		t.Fatalf("runInstall --host copilot returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".copilot", "installed-plugins", "_direct", "breakroom")
	for _, rel := range []string{
		"plugin.json",
		"mcp.json",
		filepath.Join("com.github.copilot", "hooks", "hooks.json"),
		filepath.Join("skills", "breakroom", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("expected global installed file %s via copilot alias: %v", rel, err)
		}
	}
}

func TestInstallCommandMusePlugin(t *testing.T) {
	tmp := t.TempDir()
	code := runInstall(context.Background(), []string{"--host", "muse", tmp})
	if code != 0 {
		t.Fatalf("runInstall --host muse returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".muse", "plugins", "breakroom")
	for _, check := range []struct {
		rel  string
		want []byte
	}{
		{filepath.Join(".muse-plugin", "plugin.json"), breakroom.MusePluginJSON},
		{filepath.Join(".claude-plugin", "marketplace.json"), breakroom.MuseMarketplaceJSON},
		{filepath.Join("skills", "breakroom", "SKILL.md"), breakroom.SkillMD},
		{filepath.Join("skills", "breakroom", "docs", "tool-contracts.md"), breakroom.ToolContractsMD},
		{filepath.Join("skills", "breakroom", "docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
	} {
		data, err := os.ReadFile(filepath.Join(target, check.rel))
		if err != nil {
			t.Fatalf("reading installed %s: %v", check.rel, err)
		}
		if !bytes.Equal(data, check.want) {
			t.Errorf("installed %s does not match expected embedded data", check.rel)
		}
	}
}

func TestInstallCommandMusePluginDefaultGlobal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "muse"})
	if code != 0 {
		t.Fatalf("runInstall --host muse returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".config", "muse", "plugins", "breakroom")
	for _, rel := range []string{
		filepath.Join(".muse-plugin", "plugin.json"),
		filepath.Join(".claude-plugin", "marketplace.json"),
		filepath.Join("skills", "breakroom", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("expected global installed file %s: %v", rel, err)
		}
	}
}

func TestInstallCommandClaude(t *testing.T) {
	tmp := t.TempDir()

	code := runInstall(context.Background(), []string{"--host", "claude", tmp})
	if code != 0 {
		t.Fatalf("runInstall --host claude returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".claude", "skills", "breakroom")
	for _, check := range []struct {
		rel  string
		want []byte
	}{
		{filepath.Join(".claude-plugin", "plugin.json"), breakroom.ClaudePluginJSON},
		{".mcp.json", breakroom.ClaudeMcpJSON},
		{filepath.Join("hooks", "hooks.json"), breakroom.ClaudeHooksJSON},
		{filepath.Join("skills", "breakroom", "SKILL.md"), breakroom.SkillMD},
		{filepath.Join("skills", "breakroom", "docs", "tool-contracts.md"), breakroom.ToolContractsMD},
		{filepath.Join("skills", "breakroom", "docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
	} {
		data, err := os.ReadFile(filepath.Join(target, check.rel))
		if err != nil {
			t.Fatalf("reading installed %s: %v", check.rel, err)
		}
		if !bytes.Equal(data, check.want) {
			t.Errorf("installed %s does not match expected embedded data", check.rel)
		}
	}
}

func TestInstallCommandClaudeDefaultGlobal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "claude"})
	if code != 0 {
		t.Fatalf("runInstall --host claude returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".claude", "skills", "breakroom")
	for _, rel := range []string{
		filepath.Join(".claude-plugin", "plugin.json"),
		".mcp.json",
		filepath.Join("hooks", "hooks.json"),
		filepath.Join("skills", "breakroom", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("expected global installed file %s: %v", rel, err)
		}
	}
}

func TestInstallCommandCodex(t *testing.T) {
	tmp := t.TempDir()

	code := runInstall(context.Background(), []string{"--host", "codex", tmp})
	if code != 0 {
		t.Fatalf("runInstall --host codex returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".agents", "plugins", "breakroom")
	for _, check := range []struct {
		rel  string
		want []byte
	}{
		{filepath.Join(".codex-plugin", "plugin.json"), breakroom.CodexPluginJSON},
		{".mcp.json", breakroom.CodexMcpJSON},
		{filepath.Join("hooks", "hooks.json"), breakroom.CodexHooksJSON},
		{filepath.Join("skills", "breakroom", "SKILL.md"), breakroom.SkillMD},
		{filepath.Join("skills", "breakroom", "docs", "tool-contracts.md"), breakroom.ToolContractsMD},
		{filepath.Join("skills", "breakroom", "docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
	} {
		data, err := os.ReadFile(filepath.Join(target, check.rel))
		if err != nil {
			t.Fatalf("reading installed %s: %v", check.rel, err)
		}
		if !bytes.Equal(data, check.want) {
			t.Errorf("installed %s does not match expected embedded data", check.rel)
		}
	}

	// Verify marketplace entry
	marketplaceFile := filepath.Join(tmp, ".agents", "plugins", "marketplace.json")
	mdata, err := os.ReadFile(marketplaceFile)
	if err != nil {
		t.Fatalf("reading marketplace.json: %v", err)
	}
	// Codex resolves source.path from the marketplace root (the folder
	// holding .agents/), so it must point at where the plugin was copied.
	if !strings.Contains(string(mdata), `"path": "./.agents/plugins/breakroom"`) {
		t.Errorf("marketplace.json does not point at the installed plugin: %s", string(mdata))
	}

	// The plugin ships its own hooks; no duplicate .codex/hooks.json.
	if _, err := os.Stat(filepath.Join(tmp, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Errorf(".codex/hooks.json should not be written: %v", err)
	}
}

func TestInstallCommandCodexDefaultGlobal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	code := runInstall(context.Background(), []string{"--host", "codex"})
	if code != 0 {
		t.Fatalf("runInstall --host codex returned %d, want 0", code)
	}

	target := filepath.Join(fakeHome, ".agents", "plugins", "breakroom")
	for _, rel := range []string{
		filepath.Join(".codex-plugin", "plugin.json"),
		".mcp.json",
		filepath.Join("hooks", "hooks.json"),
		filepath.Join("skills", "breakroom", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("expected global installed file %s: %v", rel, err)
		}
	}

	marketplaceFile := filepath.Join(fakeHome, ".agents", "plugins", "marketplace.json")
	if _, err := os.Stat(marketplaceFile); err != nil {
		t.Errorf("expected global marketplace.json: %v", err)
	}
}

func TestInstallCommandCodexLeavesUserHooks(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	hooksPath := filepath.Join(fakeHome, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	userHooks := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-own-hook"}]}]}}`)
	if err := os.WriteFile(hooksPath, userHooks, 0o644); err != nil {
		t.Fatal(err)
	}

	if code := runInstall(context.Background(), []string{"--host", "codex"}); code != 0 {
		t.Fatalf("runInstall --host codex returned %d, want 0", code)
	}
	got, err := os.ReadFile(hooksPath)
	if err != nil || !bytes.Equal(got, userHooks) {
		t.Errorf("~/.codex/hooks.json was modified: %s (%v)", got, err)
	}
}

func TestInstallCommandCodexPreservesExistingMarketplace(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	pluginsDir := filepath.Join(fakeHome, ".agents", "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("creating plugins dir: %v", err)
	}

	initialMarketplace := `{
  "name": "custom",
  "plugins": [
    {
      "name": "other-plugin",
      "source": { "source": "local", "path": "./plugins/other-plugin" },
      "policy": { "installation": "AVAILABLE" },
      "category": "Utilities"
    }
  ]
}
`
	marketplaceFile := filepath.Join(pluginsDir, "marketplace.json")
	if err := os.WriteFile(marketplaceFile, []byte(initialMarketplace), 0o644); err != nil {
		t.Fatalf("writing initial marketplace: %v", err)
	}

	code := runInstall(context.Background(), []string{"--host", "codex"})
	if code != 0 {
		t.Fatalf("runInstall --host codex returned %d, want 0", code)
	}

	mdata, err := os.ReadFile(marketplaceFile)
	if err != nil {
		t.Fatalf("reading marketplace: %v", err)
	}
	content := string(mdata)
	if !strings.Contains(content, "other-plugin") {
		t.Errorf("existing plugin 'other-plugin' was overwritten: %s", content)
	}
	if !strings.Contains(content, "breakroom") {
		t.Errorf("breakroom plugin was not added: %s", content)
	}
}

func TestInstallCommandCodexRefusesUnparseableMarketplace(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	pluginsDir := filepath.Join(fakeHome, ".agents", "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("creating plugins dir: %v", err)
	}

	corruptMarketplace := `{ "name": "corrupt", "plugins": [ invalid json content`
	marketplaceFile := filepath.Join(pluginsDir, "marketplace.json")
	if err := os.WriteFile(marketplaceFile, []byte(corruptMarketplace), 0o644); err != nil {
		t.Fatalf("writing corrupt marketplace: %v", err)
	}

	code := runInstall(context.Background(), []string{"--host", "codex"})
	if code == 0 {
		t.Fatalf("runInstall --host codex returned %d, want non-zero on unparseable marketplace", code)
	}

	mdata, err := os.ReadFile(marketplaceFile)
	if err != nil {
		t.Fatalf("reading marketplace: %v", err)
	}
	if string(mdata) != corruptMarketplace {
		t.Errorf("corrupt marketplace was overwritten; got %s, want %s", string(mdata), corruptMarketplace)
	}
}

func TestUpdateCodexMarketplaceUnparseable(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"unclosed object", "{\"unclosed\": true,"},
		{"unclosed array", "[{\"name\": \"test\","},
		{"invalid json", "not json at all"},
		{"bare string", "\"bare string\""},
		{"bare number", "42"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "marketplace.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}

			err := updateCodexMarketplace(path)
			if err == nil {
				t.Fatal("expected error on unparseable marketplace, got nil")
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.content {
				t.Fatalf("unparseable file was overwritten; got %q, want %q", string(data), tc.content)
			}
		})
	}
}

func TestUpdateCodexMarketplaceMissingOrEmpty(t *testing.T) {
	t.Run("missing file creates fresh catalog", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "marketplace.json")

		if err := updateCodexMarketplace(path); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"name": "breakroom"`) {
			t.Errorf("fresh catalog missing breakroom: %s", string(data))
		}
	})

	t.Run("empty file initializes fresh catalog", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "marketplace.json")
		if err := os.WriteFile(path, []byte("   \n\t  "), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := updateCodexMarketplace(path); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"name": "breakroom"`) {
			t.Errorf("fresh catalog missing breakroom: %s", string(data))
		}
	})
}

func TestInstallBinaryPathFlagAndPreservation(t *testing.T) {
	tmp := t.TempDir()
	customBin := "/opt/homebrew/bin/breakroom"

	// 1. Install with explicit --bin-path
	t.Setenv("HOME", tmp)
	code := runInstall(context.Background(), []string{"--host", "cursor", "--bin-path", customBin})
	if code != 0 {
		t.Fatalf("runInstall with --bin-path returned %d, want 0", code)
	}

	target := filepath.Join(tmp, ".cursor", "plugins", "local", "breakroom")
	mcpData, err := os.ReadFile(filepath.Join(target, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mcpData), customBin) {
		t.Fatalf("mcp.json does not contain customBin %s: %s", customBin, string(mcpData))
	}

	hooksData, err := os.ReadFile(filepath.Join(target, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hooksData), customBin+" gate") {
		t.Fatalf("hooks.json does not contain customBin gate: %s", string(hooksData))
	}

	// Verify .breakroom-version was written
	verData, err := os.ReadFile(filepath.Join(target, ".breakroom-version"))
	if err != nil {
		t.Fatal(err)
	}
	if len(verData) == 0 {
		t.Fatal(".breakroom-version is empty")
	}

	// 2. Re-install WITHOUT --bin-path: should detect and preserve customBin
	code = runInstall(context.Background(), []string{"--host", "cursor"})
	if code != 0 {
		t.Fatalf("runInstall re-install returned %d, want 0", code)
	}

	mcpData2, err := os.ReadFile(filepath.Join(target, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mcpData2), customBin) {
		t.Fatalf("re-install wiped customBin; got: %s", string(mcpData2))
	}
}

func TestUninstallCommand(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// 1. Install cursor plugin
	code := runInstall(context.Background(), []string{"--host", "cursor"})
	if code != 0 {
		t.Fatalf("runInstall returned %d", code)
	}

	cursorDir := filepath.Join(fakeHome, ".cursor", "plugins", "local", "breakroom")
	if _, err := os.Stat(cursorDir); err != nil {
		t.Fatalf("cursor plugin not installed: %v", err)
	}

	// 2. Uninstall cursor plugin
	code = runUninstall(context.Background(), []string{"--host", "cursor"})
	if code != 0 {
		t.Fatalf("runUninstall returned %d", code)
	}

	if _, err := os.Stat(cursorDir); !os.IsNotExist(err) {
		t.Fatalf("cursorDir still exists after uninstall: %v", err)
	}

	// 3. Install codex and uninstall, verifying marketplace cleanup
	code = runInstall(context.Background(), []string{"--host", "codex", fakeHome})
	if code != 0 {
		t.Fatalf("runInstall codex returned %d", code)
	}
	marketplaceFile := filepath.Join(fakeHome, ".agents", "plugins", "marketplace.json")
	mData, _ := os.ReadFile(marketplaceFile)
	if !strings.Contains(string(mData), `"breakroom"`) {
		t.Fatalf("marketplace missing breakroom: %s", string(mData))
	}

	code = runUninstall(context.Background(), []string{"--host", "codex", fakeHome})
	if code != 0 {
		t.Fatalf("runUninstall codex returned %d", code)
	}
	mData2, _ := os.ReadFile(marketplaceFile)
	if strings.Contains(string(mData2), `"breakroom"`) {
		t.Fatalf("marketplace still contains breakroom after uninstall: %s", string(mData2))
	}
}

// TestUninstallLeavesNothingRunning: every file install writes that would
// keep a hook or plugin alive is gone after uninstall, and hosts sharing a
// project plugin directory don't take each other out.
func TestUninstallLeavesNothingRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	run := func(f func(context.Context, []string) int, args ...string) {
		t.Helper()
		if code := f(context.Background(), args); code != 0 {
			t.Fatalf("%v returned %d", args, code)
		}
	}
	gone := func(paths ...string) {
		t.Helper()
		for _, p := range paths {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s still exists (err %v)", p, err)
			}
		}
	}

	// Codex and Antigravity share <proj>/.agents/plugins/breakroom.
	run(runInstall, "--host", "codex", proj)
	run(runInstall, "--host", "antigravity", proj)
	shared := filepath.Join(proj, ".agents", "plugins", "breakroom")
	run(runUninstall, "--host", "codex", proj)
	gone(filepath.Join(shared, ".codex-plugin"))
	if _, err := os.Stat(filepath.Join(shared, "mcp_config.json")); err != nil {
		t.Fatalf("uninstalling codex removed antigravity: %v", err)
	}
	run(runUninstall, "--host", "antigravity", proj)
	gone(shared)

	run(runInstall, "--host", "vscode", proj)
	run(runUninstall, "--host", "vscode", proj)
	gone(filepath.Join(proj, ".github", "hooks", "breakroom-copilot.json"))

	run(runInstall, "--host", "opencode", "--global")
	run(runUninstall, "--host", "opencode", "--global")
	gone(filepath.Join(home, ".config", "opencode", "plugins", "breakroom.ts"),
		filepath.Join(home, ".config", "opencode", "skills", "breakroom"))

	run(runInstall, "--host", "muse")
	run(runInstall, "--host", "muse", proj)
	run(runUninstall, "--host", "muse", proj)
	gone(filepath.Join(proj, ".muse", "plugins", "breakroom"))
	run(runUninstall, "--host", "muse")
	gone(filepath.Join(home, ".config", "muse", "plugins", "breakroom"))
}

func TestSkillCopyDrift(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting wd: %v", err)
	}
	repoRoot := wd
	if _, err := os.Stat(filepath.Join(repoRoot, "SKILL.md")); os.IsNotExist(err) {
		repoRoot = filepath.Dir(wd)
		if _, err := os.Stat(filepath.Join(repoRoot, "SKILL.md")); os.IsNotExist(err) {
			repoRoot = filepath.Dir(repoRoot)
		}
	}

	patterns := []string{
		filepath.Join(repoRoot, "integrations", "*", "breakroom-plugin", "skills", "breakroom"),
	}

	found := 0
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, dir := range matches {
			found++
			host := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(dir))))

			for _, check := range []struct {
				rel  string
				want []byte
			}{
				{"SKILL.md", breakroom.SkillMD},
				{filepath.Join("docs", "tool-contracts.md"), breakroom.ToolContractsMD},
				{filepath.Join("docs", "blueprint-spec.md"), breakroom.BlueprintSpecMD},
			} {
				filePath := filepath.Join(dir, check.rel)
				data, err := os.ReadFile(filePath)
				if err != nil {
					t.Errorf("host %q missing %s: %v", host, check.rel, err)
					continue
				}
				if !bytes.Equal(data, check.want) {
					t.Errorf("host %q file %s has drifted from canonical root copy", host, check.rel)
				}
			}
		}
	}
	if found == 0 {
		t.Fatalf("expected to find checked-in plugin skill directories in %s", repoRoot)
	}
}

func TestSelectMenuCtrlC(t *testing.T) {
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	go func() {
		defer w.Close()
		_, _ = w.Write([]byte{3})
	}()

	_, err = selectMenu([]string{"test header"}, []string{"option 1", "option 2"})
	if !errors.Is(err, errAborted) {
		t.Fatalf("selectMenu on Ctrl+C = %v, want errAborted", err)
	}
}

func TestReadLineWithInterruptCtrlC(t *testing.T) {
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	go func() {
		defer w.Close()
		_, _ = w.Write([]byte{3})
	}()

	_, err = readLineWithInterrupt("test: ")
	if !errors.Is(err, errAborted) {
		t.Fatalf("readLineWithInterrupt on Ctrl+C = %v, want errAborted", err)
	}
}

func TestReadLineWithInterruptInput(t *testing.T) {
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	go func() {
		defer w.Close()
		_, _ = w.Write([]byte("custom/dir\n"))
	}()

	line, err := readLineWithInterrupt("test: ")
	if err != nil {
		t.Fatalf("readLineWithInterrupt error: %v", err)
	}
	if line != "custom/dir" {
		t.Fatalf("readLineWithInterrupt = %q, want %q", line, "custom/dir")
	}
}

func TestProjectOrGlobalPrompt(t *testing.T) {
	got := projectOrGlobalPrompt(findHostTarget("antigravity"))
	want := "Project directory (installs to <dir>/.agents/plugins/breakroom) [blank = global: ~/.gemini/config/plugins/breakroom]: "
	if got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

// Muse uninstall strips a manual setup from the user's own config files:
// Breakroom's MCP server and hooks go, everything else stays in its order
// (schema_version first), and a project hooks file left empty is deleted.
func TestUninstallMuseManualSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	settings := filepath.Join(home, ".config", "muse", "settings.json")
	projHooks := filepath.Join(proj, ".muse", "hooks.json")
	for path, body := range map[string]string{
		settings: `{
  "schema_version": 1,
  "model": "m",
  "mcpServers": {
    "trello": {"url": "https://mcp.trello.com/v1"},
    "breakroom": {"command": "breakroom", "args": ["mcp"]}
  },
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "/opt/homebrew/bin/breakroom gate --event stop --host muse"},
                 {"type": "command", "command": "notify-send done"}]}
    ],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "breakroom gate --event prompt --host muse"}]}]
  }
}`,
		projHooks: `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "breakroom gate --event stop --host muse"}]}]}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if code := runUninstall(context.Background(), []string{"--host", "muse"}); code != 0 {
		t.Fatalf("global uninstall returned %d", code)
	}
	got, _ := os.ReadFile(settings)
	want := `{
  "schema_version": 1,
  "model": "m",
  "mcpServers": {
    "trello": {
      "url": "https://mcp.trello.com/v1"
    }
  },
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "notify-send done"
          }
        ]
      }
    ]
  }
}
`
	if string(got) != want {
		t.Errorf("settings.json after uninstall =\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(projHooks); err != nil {
		t.Fatalf("global uninstall touched the project hooks: %v", err)
	}

	if code := runUninstall(context.Background(), []string{"--host", "muse", proj}); code != 0 {
		t.Fatalf("project uninstall returned %d", code)
	}
	if _, err := os.Stat(projHooks); !os.IsNotExist(err) {
		t.Errorf("project hooks.json left behind with only Breakroom removed: %v", err)
	}
}

// TestInstallUninstallRoundTrips covers bugs found in review: each install
// is fully undone, and paths survive spaces and different spellings.
func TestInstallUninstallRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	run := func(f func(context.Context, []string) int, args ...string) {
		t.Helper()
		if code := f(context.Background(), args); code != 0 {
			t.Fatalf("%v returned %d", args, code)
		}
	}

	t.Run("vscode settings key is workspace-relative", func(t *testing.T) {
		run(runInstall, "--host", "vscode", proj)
		settings := filepath.Join(proj, ".vscode", "settings.json")
		data, _ := os.ReadFile(settings)
		if !strings.Contains(string(data), `".vscode/plugins/breakroom": true`) {
			t.Fatalf("settings.json = %s, want a relative plugin key", data)
		}
		t.Chdir(proj)
		run(runUninstall, "--host", "vscode", ".")
		if _, err := os.Stat(settings); !os.IsNotExist(err) {
			data, _ := os.ReadFile(settings)
			t.Errorf("settings.json left after uninstall: %s", data)
		}
	})

	t.Run("bin path with a space", func(t *testing.T) {
		bin := filepath.Join(home, "My Tools", "breakroom")
		run(runInstall, "--host", "claude", "--bin-path", bin)
		target := filepath.Join(home, ".claude", "skills", "breakroom")
		hooks, _ := os.ReadFile(filepath.Join(target, "hooks", "hooks.json"))
		if !strings.Contains(string(hooks), `"command": "'`+bin+`' gate`) {
			t.Fatalf("hook command not shell-quoted: %s", hooks)
		}
		if got := detectExistingBinary(target); got != bin {
			t.Errorf("detectExistingBinary = %q, want %q", got, bin)
		}
		if !runsBreakroom([]byte(`{"command": "'` + bin + `' gate --event stop"}`)) {
			t.Error("runsBreakroom missed a quoted binary path")
		}
	})

	t.Run("opencode doctor and uninstall", func(t *testing.T) {
		run(runInstall, "--host", "opencode")
		found := false
		for _, p := range checkHostPlugins(home) {
			if p.Host == "opencode" {
				found = true
				if p.Status != "installed (up to date)" {
					t.Errorf("opencode status = %q", p.Status)
				}
			}
		}
		if !found {
			t.Error("doctor did not list opencode")
		}
		run(runUninstall, "--host", "opencode")
		if _, err := os.Stat(filepath.Join(home, ".config", "opencode", ".breakroom-version")); !os.IsNotExist(err) {
			t.Errorf(".breakroom-version left after uninstall: %v", err)
		}
	})

	t.Run("uninstall all keeps the project generic skill", func(t *testing.T) {
		t.Chdir(proj)
		run(runInstall, "--host", "generic")
		run(runUninstall, "--host", "all")
		if _, err := os.Stat(filepath.Join(proj, ".agents", "skills", "breakroom", "SKILL.md")); err != nil {
			t.Errorf("uninstall --host all removed the project skill: %v", err)
		}
	})

	t.Run("codex marketplace removed when emptied", func(t *testing.T) {
		run(runInstall, "--host", "codex", proj)
		run(runUninstall, "--host", "codex", proj)
		if _, err := os.Stat(filepath.Join(proj, ".agents", "plugins", "marketplace.json")); !os.IsNotExist(err) {
			t.Errorf("empty marketplace.json left behind: %v", err)
		}
	})
}

// install.sh installs a download only when it matches checksums.txt.
func TestInstallScriptVerifiesChecksum(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if (goos != "darwin" && goos != "linux") || (goarch != "amd64" && goarch != "arm64") {
		t.Skip("unsupported platform for install.sh")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "breakroom"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := fmt.Sprintf("breakroom_1.0.0_%s_%s.tar.gz", goos, goarch)
	if out, err := exec.Command("tar", "-czf", filepath.Join(dir, tarball), "-C", dir, "breakroom").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, tarball))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	run := func(checksums string) (string, error) {
		sums := filepath.Join(dir, "checksums.txt")
		if err := os.WriteFile(sums, []byte(checksums), 0o644); err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		cmd := exec.Command("bash", "../../install.sh")
		cmd.Env = append(os.Environ(),
			"BREAKROOM_VERSION=1.0.0", "BREAKROOM_INSTALL_DIR="+bin,
			"BREAKROOM_DOWNLOAD_URL=file://"+filepath.Join(dir, tarball),
			"BREAKROOM_CHECKSUMS_URL=file://"+sums)
		out, err := cmd.CombinedOutput()
		if _, statErr := os.Stat(filepath.Join(bin, "breakroom")); (statErr == nil) != (err == nil) {
			t.Fatalf("installed = %v but exit err = %v: %s", statErr == nil, err, out)
		}
		return string(out), err
	}
	if out, err := run(hex.EncodeToString(sum[:]) + "  " + tarball + "\n"); err != nil {
		t.Fatalf("good checksum: %v %s", err, out)
	} else if strings.Contains(out, "sudo") {
		t.Fatalf("custom install dir mentions sudo: %s", out)
	}
	if out, err := run(strings.Repeat("0", 64) + "  " + tarball + "\n"); err == nil || !strings.Contains(out, "mismatch") {
		t.Fatalf("bad checksum: err = %v %s", err, out)
	}
	if out, err := run(hex.EncodeToString(sum[:]) + "  other.tar.gz\n"); err == nil || !strings.Contains(out, "mismatch") {
		t.Fatalf("unlisted archive: err = %v %s", err, out)
	}
}
