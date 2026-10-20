package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/search-3d/breakroom"
)

// installAntigravity writes the Antigravity plugin: MCP server, hooks, and skill.
func installAntigravity(o installOpts) error {
	files := skillFiles()
	files["plugin.json"] = breakroom.AntigravityPluginJSON
	files["mcp_config.json"] = breakroom.AntigravityMcpConfigJSON
	files["hooks.json"] = breakroom.AntigravityHooksJSON
	if err := writeFiles(o.target, files, o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	printCopied(o.target, append([]string{"plugin.json", "mcp_config.json", "hooks.json"}, skillFileList()...)...)
	fmt.Println()
	if o.global {
		fmt.Println("Breakroom is now configured as a global Antigravity plugin. The MCP server, lifecycle hooks, and skills load in every workspace.")
	} else {
		fmt.Println("Breakroom is now configured as an Antigravity plugin. The MCP server, lifecycle hooks, and skills load in this workspace only.")
	}
	return nil
}

// installCursor writes the global Cursor plugin, or project config when a
// project is given (Cursor loads plugins only from the global folder).
func installCursor(o installOpts) error {
	if !o.global {
		return installCursorProjectScope(o)
	}
	files := skillFiles()
	files["plugin.json"] = breakroom.CursorPluginJSON
	files["mcp.json"] = breakroom.CursorMcpJSON
	files[filepath.Join("hooks", "hooks.json")] = breakroom.CursorHooksJSON
	if err := writeFiles(o.target, files, o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	printCopied(o.target, append([]string{"plugin.json", "mcp.json", "hooks/hooks.json"}, skillFileList()...)...)
	fmt.Println()
	fmt.Println("Breakroom is now configured as a global Cursor plugin. The MCP server, lifecycle hooks, and skills load in every workspace.")
	fmt.Println("Tip: Restart Cursor or run 'Developer: Reload Window' to load the plugin.")
	return nil
}

// installClaude writes the Claude Code plugin into a skills folder, where
// Claude Code loads it without a marketplace.
func installClaude(o installOpts) error {
	files := skillFiles()
	files[filepath.Join(".claude-plugin", "plugin.json")] = breakroom.ClaudePluginJSON
	files[".mcp.json"] = breakroom.ClaudeMcpJSON
	files[filepath.Join("hooks", "hooks.json")] = breakroom.ClaudeHooksJSON
	if err := writeFiles(o.target, files, o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	printCopied(o.target, append([]string{".claude-plugin/plugin.json", ".mcp.json", "hooks/hooks.json"}, skillFileList()...)...)
	fmt.Println()
	if o.global {
		fmt.Println("Breakroom is now configured as a global Claude Code plugin. The MCP server, lifecycle hooks, and skills load in every workspace.")
	} else {
		fmt.Println("Breakroom is now configured as a Claude Code plugin. The MCP server, lifecycle hooks, and skills load in this workspace only.")
	}
	fmt.Println()
	fmt.Println("Permissions note:")
	fmt.Println("Claude Code prefixes plugin MCP tools with mcp__plugin_breakroom_breakroom__*.")
	fmt.Println("To auto-approve Breakroom tools, add to your ~/.claude/settings.json:")
	fmt.Println(`  "permissions": { "allow": ["mcp__plugin_breakroom_breakroom"] }`)
	return nil
}

// installCodex writes the Codex plugin and registers it in the marketplace
// file Codex reads beside it.
func installCodex(o installOpts) error {
	files := skillFiles()
	files[filepath.Join(".codex-plugin", "plugin.json")] = breakroom.CodexPluginJSON
	files[".mcp.json"] = breakroom.CodexMcpJSON
	files[filepath.Join("hooks", "hooks.json")] = breakroom.CodexHooksJSON
	if err := writeFiles(o.target, files, o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	marketplacePath := filepath.Join(filepath.Dir(o.target), "marketplace.json")
	if err := updateCodexMarketplace(marketplacePath); err != nil {
		return fmt.Errorf("failed to update marketplace: %w", err)
	}

	printCopied(o.target, append([]string{".codex-plugin/plugin.json", ".mcp.json", "hooks/hooks.json"}, skillFileList()...)...)
	fmt.Printf("Registered plugin in marketplace: %s\n", marketplacePath)
	fmt.Println()
	if o.global {
		fmt.Println("Breakroom is now configured as a global Codex plugin.")
	} else {
		fmt.Println("Breakroom is now configured as a Codex plugin for this project.")
	}
	fmt.Println()
	fmt.Println("To activate the plugin in Codex:")
	fmt.Println("1. Run codex, then open /plugins and enable 'breakroom'.")
	fmt.Println("2. Run /hooks to review and trust the plugin lifecycle hooks.")
	fmt.Println("3. Keep tool_timeout_sec >= 60 in ~/.codex/config.toml.")
	return nil
}

// updateCodexMarketplace adds (or replaces) the breakroom entry in a Codex
// marketplace file, creating the file when missing or empty.
func updateCodexMarketplace(marketplacePath string) error {
	entry := map[string]any{
		"name": "breakroom",
		"source": map[string]any{
			"source": "local",
			// Relative to the marketplace root, the folder holding .agents/.
			"path": "./.agents/plugins/breakroom",
		},
		"policy": map[string]any{
			"installation":   "AVAILABLE",
			"authentication": "ON_INSTALL",
		},
		"category": "Productivity",
	}

	if err := os.MkdirAll(filepath.Dir(marketplacePath), 0o755); err != nil {
		return err
	}

	// Default for a missing or empty file: a fresh catalog.
	var catalog any = map[string]any{
		"name":    "local",
		"plugins": []any{entry},
	}

	data, err := os.ReadFile(marketplacePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 {
		var rawObj map[string]any
		var rawArr []any
		errObj := json.Unmarshal(data, &rawObj)
		if errObj == nil && rawObj != nil {
			pluginsList, _ := rawObj["plugins"].([]any)
			rawObj["plugins"] = upsertPlugin(pluginsList, entry)
			catalog = rawObj
		} else if errArr := json.Unmarshal(data, &rawArr); errArr == nil {
			catalog = upsertPlugin(rawArr, entry)
		} else {
			parseErr := errObj
			if trimmed[0] == '[' {
				parseErr = errArr
			}
			return fmt.Errorf("parsing existing %s: %w", marketplacePath, parseErr)
		}
	}

	out, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(marketplacePath, append(out, '\n'), 0o644)
}

// upsertPlugin replaces the plugin entry with the same name, or appends it.
func upsertPlugin(list []any, entry map[string]any) []any {
	for i, p := range list {
		if pm, ok := p.(map[string]any); ok && pm["name"] == entry["name"] {
			list[i] = entry
			return list
		}
	}
	return append(list, entry)
}

// installOpenCode writes the OpenCode plugin (breakroom.ts) and the skill
// into the OpenCode config folder.
func installOpenCode(o installOpts) error {
	opencodeDir := versionDir(o.host, o.target)
	files := skillFiles()
	files[filepath.Join("plugins", "breakroom.ts")] = breakroom.OpenCodePluginTS
	if err := writeFiles(opencodeDir, files, o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	printCopied(opencodeDir, append([]string{"plugins/breakroom.ts"}, skillFileList()...)...)
	fmt.Println()
	if o.global {
		fmt.Println("Breakroom is now configured as a global OpenCode plugin. The MCP server, lifecycle hooks, and skills load in every workspace.")
	} else {
		fmt.Println("Breakroom is now configured as an OpenCode plugin. The MCP server, lifecycle hooks, and skills load in this workspace only.")
	}
	return nil
}

// vscodePluginKey is the chat.pluginLocations entry for a project install:
// workspace-relative, so the settings file works on every machine.
func vscodePluginKey(host *HostTarget) string {
	return filepath.ToSlash(projectRel(host))
}

// installVSCode writes the VS Code plugin, the Copilot harness hook file,
// and (for a project) enables the plugin in .vscode/settings.json.
func installVSCode(o installOpts) error {
	files := skillFiles()
	files["plugin.json"] = breakroom.VSCodePluginJSON
	files["mcp.json"] = breakroom.VSCodeMcpJSON
	files[filepath.Join("com.github.copilot", "hooks", "hooks.json")] = breakroom.VSCodeHooksJSON
	if err := writeFiles(o.target, files, o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	printCopied(o.target, append([]string{"plugin.json", "mcp.json", "com.github.copilot/hooks/hooks.json"}, skillFileList()...)...)

	// The Copilot harness ignores plugin hooks; its own hook file lives outside the plugin.
	root := projectRoot(o.host, o.target)
	hooksDir := filepath.Join(root, ".github", "hooks")
	if o.global {
		hooksDir = filepath.Join(o.home, ".copilot", "hooks")
	}
	bin := o.binPath
	if bin == "" {
		bin = detectExistingBinary(o.target)
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", hooksDir, err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "breakroom-copilot.json"), adjustBinaryPath(breakroom.VSCodeCopilotHooksJSON, bin), 0o644); err != nil {
		return fmt.Errorf("writing Copilot hooks: %w", err)
	}
	printCopied(hooksDir, "breakroom-copilot.json")

	fmt.Println()
	if o.global {
		fmt.Println("Breakroom is now configured as a global VS Code / Copilot plugin. The MCP server, lifecycle hooks, and skills load in every workspace.")
	} else {
		fmt.Println("Breakroom is now configured as a VS Code / Copilot plugin.")
		settings := filepath.Join(root, ".vscode", "settings.json")
		key := vscodePluginKey(o.host)
		if ok, err := updateVSCodePluginSettings(settings, key, true); err != nil {
			return fmt.Errorf("updating %s: %w", settings, err)
		} else if ok {
			fmt.Printf("Enabled it for this workspace in %s (chat.pluginLocations).\n", settings)
		} else {
			fmt.Printf("Could not edit %s safely. To enable it in this workspace, add:\n", settings)
			fmt.Printf("  \"chat.pluginLocations\": {\n    \"%s\": true\n  }\n", key)
		}
	}
	fmt.Println("Tip: Reload VS Code window to discover the plugin.")
	fmt.Println("Then start the MCP server: Cmd+Shift+P -> \"MCP: List Servers\" -> breakroom -> Start Server.")
	return nil
}

// installMuse writes the Muse plugin bundle and prints the muse commands
// that activate it. It warns about a manual setup that would double the hooks.
func installMuse(o installOpts) error {
	files := skillFiles()
	files[filepath.Join(".muse-plugin", "plugin.json")] = breakroom.MusePluginJSON
	files[filepath.Join(".claude-plugin", "marketplace.json")] = breakroom.MuseMarketplaceJSON
	if err := writeFiles(o.target, files, o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	printCopied(o.target, append([]string{".muse-plugin/plugin.json", ".claude-plugin/marketplace.json"}, skillFileList()...)...)
	scope := "user"
	if !o.global {
		scope = "project"
	}
	fmt.Println()
	fmt.Println("Breakroom is now packaged as a Muse Code plugin. To activate it:")
	fmt.Printf("1. muse plugins install %s --scope %s\n", o.target, scope)
	fmt.Println("2. muse plugins approve breakroom   (trusts its MCP server and hooks; without it the plugin is inert)")
	fmt.Println("3. Verify in Muse with /plugins and /mcp.")
	fmt.Println("After a later `breakroom install`, refresh Muse's copy with: muse plugins update breakroom")
	// A manual setup left beside the plugin runs every gate twice.
	configs := []string{museManualConfig(true, o.home, "")}
	if !o.global {
		configs = append(configs, museManualConfig(false, o.home, projectRoot(o.host, o.target)))
	}
	for _, path := range configs {
		if found, err := stripBreakroomJSON(path, false); err == nil && len(found) > 0 {
			fmt.Printf("\nFound a manual Breakroom setup in %s (%s); remove it, or every hook runs twice.\n", path, strings.Join(found, ", "))
			fmt.Println("`breakroom uninstall --host muse` removes it along with the plugin.")
		}
	}
	return nil
}

// museManualConfig is where a manual (non-plugin) Breakroom setup lives:
// ~/.config/muse/settings.json for the user, <project>/.muse/hooks.json for a project.
func museManualConfig(global bool, home, project string) string {
	if global {
		return filepath.Join(home, ".config", "muse", "settings.json")
	}
	return filepath.Join(project, ".muse", "hooks.json")
}

// stripBreakroomJSON removes the breakroom MCP server and every hook that
// runs breakroom from a JSON config file, keeping the rest in its key order.
// It returns what it found; with write false it only reports.
func stripBreakroomJSON(path string, write bool) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var root orderedObject
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var found []string
	var servers orderedObject
	if raw, ok := root.vals["mcpServers"]; ok && json.Unmarshal(raw, &servers) == nil && servers.vals["breakroom"] != nil {
		servers.remove("breakroom")
		root.setObject("mcpServers", servers)
		found = append(found, "mcpServers.breakroom")
	}
	var hooks orderedObject
	if raw, ok := root.vals["hooks"]; ok && json.Unmarshal(raw, &hooks) == nil {
		changed := false
		for _, event := range slices.Clone(hooks.keys) {
			var groups []json.RawMessage
			if json.Unmarshal(hooks.vals[event], &groups) != nil {
				continue
			}
			kept, n := pruneBreakroomHooks(groups)
			if n == 0 {
				continue
			}
			found, changed = append(found, "hooks."+event), true
			if len(kept) == 0 {
				hooks.remove(event)
			} else {
				hooks.vals[event], _ = json.Marshal(kept)
			}
		}
		if changed {
			root.setObject("hooks", hooks)
		}
	}
	if !write || len(found) == 0 {
		return found, nil
	}
	return found, writeOrderedJSON(path, root)
}

// pruneBreakroomHooks drops hook entries that run breakroom from one
// event's groups, and groups left empty by that. It handles both the
// grouped shape ({"hooks": [{"command": ...}]}) and flat entries.
func pruneBreakroomHooks(groups []json.RawMessage) (kept []json.RawMessage, removed int) {
	for _, g := range groups {
		if runsBreakroom(g) {
			removed++
			continue
		}
		var group orderedObject
		var inner []json.RawMessage
		if json.Unmarshal(g, &group) != nil || json.Unmarshal(group.vals["hooks"], &inner) != nil {
			kept = append(kept, g)
			continue
		}
		keptInner := slices.DeleteFunc(slices.Clone(inner), runsBreakroom)
		if n := len(inner) - len(keptInner); n > 0 {
			removed += n
			if len(keptInner) == 0 {
				continue
			}
			group.vals["hooks"], _ = json.Marshal(keptInner)
			g, _ = json.Marshal(group)
		}
		kept = append(kept, g)
	}
	return kept, removed
}

// runsBreakroom reports whether a hook entry's command (a shell string or
// an argv array) starts with the breakroom binary.
func runsBreakroom(entry json.RawMessage) bool {
	var h struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal(entry, &h) != nil || h.Command == nil {
		return false
	}
	var first, line string
	var argv []string
	switch {
	case json.Unmarshal(h.Command, &line) == nil:
		first = firstShellWord(line)
	case json.Unmarshal(h.Command, &argv) == nil && len(argv) > 0:
		first = argv[0]
	}
	return filepath.Base(first) == "breakroom"
}

// firstShellWord returns the first word of a shell command line, keeping a
// leading quoted path (one with spaces) whole.
func firstShellWord(line string) string {
	line = strings.TrimSpace(line)
	if line != "" && (line[0] == '\'' || line[0] == '"') {
		if end := strings.IndexByte(line[1:], line[0]); end >= 0 {
			return line[1 : end+1]
		}
	}
	if f := strings.Fields(line); len(f) > 0 {
		return f[0]
	}
	return ""
}

// orderedObject is a JSON object that keeps its key order through an edit.
type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

// UnmarshalJSON reads a JSON object, remembering the order of its keys.
func (o *orderedObject) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	o.keys, o.vals = nil, map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return err
		}
		if _, dup := o.vals[key]; !dup {
			o.keys = append(o.keys, key)
		}
		o.vals[key] = val
	}
	_, err := dec.Token()
	return err
}

// MarshalJSON writes the object with its keys in their original order.
func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// remove deletes key from the object.
func (o *orderedObject) remove(key string) {
	delete(o.vals, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
}

// setObject stores v under key, or drops key when v is empty.
func (o *orderedObject) setObject(key string, v orderedObject) {
	if len(v.keys) == 0 {
		o.remove(key)
		return
	}
	o.vals[key], _ = json.Marshal(v)
}

// installGeneric writes just the skill and its docs, for hosts without a plugin.
func installGeneric(o installOpts) error {
	if err := writeFiles(o.target, skillDocs(), o.binPath); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}

	printCopied(o.target, skillDocList...)
	fmt.Println()
	fmt.Printf("Breakroom is now installed as a skill for %s. Add the MCP server (and hooks, if the host has them) yourself.\n", o.host.Name)
	return nil
}
