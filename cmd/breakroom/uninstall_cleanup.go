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
)

// set stores v under key, keeping the key's position if it already exists.
func (o *orderedObject) set(key string, v any) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key], _ = json.Marshal(v)
}

// writeOrderedJSON writes root indented, or deletes the file when root is empty.
func writeOrderedJSON(path string, root orderedObject) error {
	if len(root.keys) == 0 {
		return os.Remove(path)
	}
	compact, err := json.Marshal(root)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	return os.WriteFile(path, out.Bytes(), 0o644)
}

// readOrderedJSON reads a JSON object file; a missing or blank file is an empty object.
func readOrderedJSON(path string) (orderedObject, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(data)) == 0) {
		return orderedObject{vals: map[string]json.RawMessage{}}, false, nil
	}
	if err != nil {
		return orderedObject{}, false, err
	}
	var root orderedObject
	if err := json.Unmarshal(data, &root); err != nil {
		return orderedObject{}, true, fmt.Errorf("parsing %s: %w", path, err)
	}
	return root, true, nil
}

// isBreakroomPermission reports whether an Antigravity permission grant
// (e.g. "mcp(breakroom/blueprint_land)", "command(breakroom)") was for Breakroom.
func isBreakroomPermission(s string) bool {
	if strings.HasPrefix(s, "mcp(breakroom") || strings.Contains(s, "plugins/breakroom") {
		return true
	}
	rest, ok := strings.CutPrefix(s, "command(breakroom")
	return ok && (rest == ")" || strings.HasPrefix(rest, " "))
}

// stripAntigravitySettings removes Breakroom's grants from the permission
// lists of the Antigravity CLI settings file, returning how many it removed.
func stripAntigravitySettings(path string) (int, error) {
	root, exists, err := readOrderedJSON(path)
	if err != nil || !exists {
		return 0, err
	}
	var perms orderedObject
	if raw, ok := root.vals["permissions"]; !ok || json.Unmarshal(raw, &perms) != nil {
		return 0, nil
	}
	removed := 0
	for _, key := range slices.Clone(perms.keys) {
		var list []json.RawMessage
		if json.Unmarshal(perms.vals[key], &list) != nil {
			continue
		}
		kept := slices.DeleteFunc(slices.Clone(list), func(e json.RawMessage) bool {
			var s string
			return json.Unmarshal(e, &s) == nil && isBreakroomPermission(s)
		})
		if len(kept) != len(list) {
			removed += len(list) - len(kept)
			perms.vals[key], _ = json.Marshal(kept)
		}
	}
	if removed == 0 {
		return 0, nil
	}
	root.setObject("permissions", perms)
	return removed, writeOrderedJSON(path, root)
}

// cleanAntigravityGlobal removes what a global Antigravity install leaves
// outside the plugin directory: permission grants, cached MCP tool schemas,
// plugin data, and a manual MCP server entry.
func cleanAntigravityGlobal(home string) {
	gemini := filepath.Join(home, ".gemini")
	settings := filepath.Join(gemini, "antigravity-cli", "settings.json")
	if n, err := stripAntigravitySettings(settings); err != nil {
		fmt.Fprintf(os.Stderr, "breakroom uninstall: %v\n", err)
	} else if n > 0 {
		fmt.Printf("Removed %d Breakroom permission grants from %s\n", n, settings)
	}
	for _, pattern := range []string{
		filepath.Join(gemini, "antigravity-cli", "mcp", "breakroom*"),
		filepath.Join(gemini, "antigravity-ide", "mcp", "breakroom*"),
		filepath.Join(gemini, "antigravity-cli", "plugin_data", "breakroom*"),
		filepath.Join(gemini, "antigravity-ide", "plugin_data", "breakroom*"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if err := os.RemoveAll(m); err == nil {
				fmt.Printf("Removed %s\n", m)
			}
		}
	}
	mcpCfg := filepath.Join(gemini, "config", "mcp_config.json")
	root, exists, err := readOrderedJSON(mcpCfg)
	if err != nil || !exists {
		return
	}
	var servers orderedObject
	if raw, ok := root.vals["mcpServers"]; ok && json.Unmarshal(raw, &servers) == nil && servers.vals["breakroom"] != nil {
		servers.remove("breakroom")
		root.setObject("mcpServers", servers)
		if err := writeOrderedJSON(mcpCfg, root); err == nil {
			fmt.Printf("Removed the breakroom MCP server from %s\n", mcpCfg)
		}
	}
}

// stripCodexConfig removes Breakroom's tables from a Codex config.toml: the
// plugin enablement, its trusted hook hashes, and a manual MCP server. It
// returns the removed table headers.
func stripCodexConfig(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed, kept []string
	skipping := false
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "[") {
			skipping = isBreakroomCodexTable(t)
			if skipping {
				removed = append(removed, t)
			}
		}
		if !skipping {
			kept = append(kept, line)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	return removed, os.WriteFile(path, []byte(strings.Join(kept, "")), 0o644)
}

// isBreakroomCodexTable reports whether a config.toml table header is one
// of Breakroom's (plugin enablement, trusted hooks, or MCP server).
func isBreakroomCodexTable(header string) bool {
	for _, p := range []string{`[plugins."breakroom@`, `[hooks.state."breakroom@`, `[mcp_servers.breakroom]`, `[mcp_servers.breakroom.`} {
		if strings.HasPrefix(header, p) {
			return true
		}
	}
	return false
}

// updateVSCodePluginSettings adds or removes the plugin in chat.pluginLocations
// of a workspace settings.json. It reports false when the file can't be edited
// safely (say, it has comments), so the caller can print manual steps.
func updateVSCodePluginSettings(path, pluginDir string, enable bool) (bool, error) {
	root, exists, err := readOrderedJSON(path)
	if err != nil {
		return false, nil
	}
	if !exists && !enable {
		return true, nil
	}
	var locs orderedObject
	if raw, ok := root.vals["chat.pluginLocations"]; ok {
		if json.Unmarshal(raw, &locs) != nil {
			return false, nil
		}
	}
	if enable {
		locs.set(pluginDir, true)
		root.set("chat.pluginLocations", locs)
		if _, ok := root.vals["chat.plugins.enabled"]; !ok {
			root.set("chat.plugins.enabled", true)
		}
	} else {
		if locs.vals == nil || locs.vals[pluginDir] == nil {
			return true, nil
		}
		locs.remove(pluginDir)
		root.setObject("chat.pluginLocations", locs)
		if len(locs.keys) == 0 {
			// Install turned plugins on for this one; nothing else needs it now.
			root.remove("chat.plugins.enabled")
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, writeOrderedJSON(path, root)
}
