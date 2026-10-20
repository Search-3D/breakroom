package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/search-3d/breakroom"
)

// stripClaudeLocalSettings removes Breakroom's MCP approval from a Claude Code
// settings.local.json, deleting the file when nothing else is left.
func stripClaudeLocalSettings(path string) (bool, error) {
	root, exists, err := readOrderedJSON(path)
	if err != nil || !exists {
		return false, err
	}
	var list []string
	if raw, ok := root.vals["enabledMcpjsonServers"]; !ok || json.Unmarshal(raw, &list) != nil {
		return false, nil
	}
	kept := slices.DeleteFunc(slices.Clone(list), func(s string) bool {
		return strings.HasPrefix(s, "plugin:breakroom:") || s == "breakroom"
	})
	if len(kept) == len(list) {
		return false, nil
	}
	if len(kept) == 0 {
		root.remove("enabledMcpjsonServers")
	} else {
		root.set("enabledMcpjsonServers", kept)
	}
	return true, writeOrderedJSON(path, root)
}

// installCursorProjectScope installs into <project>/.cursor as native
// project config (mcp.json, hooks.json, and a skill), since Cursor loads
// plugins only from the global folder.
func installCursorProjectScope(o installOpts) error {
	cursorDir := filepath.Join(projectRoot(o.host, o.target), ".cursor")
	bin := o.binPath
	if bin == "" {
		bin = detectExistingBinary(cursorDir)
	}
	mcp := adjustBinaryPath(breakroom.CursorMcpJSON, bin)
	hooks := adjustBinaryPath(breakroom.CursorHooksJSON, bin)
	if err := mergeCursorProject(cursorDir, mcp, hooks); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	if err := writeFiles(o.target, skillDocs(), bin); err != nil {
		return fmt.Errorf("failed to write files: %w", err)
	}
	printCopied(cursorDir, append([]string{"mcp.json (breakroom server merged)", "hooks.json (breakroom hooks merged)"}, skillDocList...)...)
	fmt.Println()
	fmt.Println("Breakroom is now configured for Cursor in this project only. Cursor loads plugins just from the global folder, so the MCP server, hooks, and skill are installed as project config.")
	fmt.Println("Tip: Restart Cursor or run 'Developer: Reload Window', then enable the breakroom server in Cursor Settings -> MCP.")
	return nil
}

// mergeCursorProject adds Breakroom's MCP server and hooks to the project's
// .cursor/mcp.json and .cursor/hooks.json, keeping everything already there.
func mergeCursorProject(cursorDir string, mcp, hooks []byte) error {
	var newMCP, newHooks orderedObject
	if err := json.Unmarshal(mcp, &newMCP); err != nil {
		return err
	}
	if err := json.Unmarshal(hooks, &newHooks); err != nil {
		return err
	}
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		return err
	}

	mcpPath := filepath.Join(cursorDir, "mcp.json")
	root, _, err := readOrderedJSON(mcpPath)
	if err != nil {
		return err
	}
	var servers, ours orderedObject
	if raw, ok := root.vals["mcpServers"]; ok {
		_ = json.Unmarshal(raw, &servers)
	}
	if err := json.Unmarshal(newMCP.vals["mcpServers"], &ours); err != nil {
		return err
	}
	servers.set("breakroom", json.RawMessage(ours.vals["breakroom"]))
	root.set("mcpServers", servers)
	if err := writeOrderedJSON(mcpPath, root); err != nil {
		return err
	}

	hooksPath := filepath.Join(cursorDir, "hooks.json")
	root, _, err = readOrderedJSON(hooksPath)
	if err != nil {
		return err
	}
	if _, ok := root.vals["version"]; !ok {
		root.set("version", json.RawMessage(newHooks.vals["version"]))
	}
	var existing, ourHooks orderedObject
	if raw, ok := root.vals["hooks"]; ok {
		_ = json.Unmarshal(raw, &existing)
	}
	if err := json.Unmarshal(newHooks.vals["hooks"], &ourHooks); err != nil {
		return err
	}
	for _, event := range ourHooks.keys {
		var entries, add []json.RawMessage
		if raw, ok := existing.vals[event]; ok {
			_ = json.Unmarshal(raw, &entries)
		}
		entries, _ = pruneBreakroomHooks(entries)
		_ = json.Unmarshal(ourHooks.vals[event], &add)
		existing.set(event, append(entries, add...))
	}
	root.set("hooks", existing)
	return writeOrderedJSON(hooksPath, root)
}

// removeCursorProject undoes mergeCursorProject, leaving other entries alone.
func removeCursorProject(cursorDir string) {
	for _, name := range []string{"mcp.json", "hooks.json"} {
		path := filepath.Join(cursorDir, name)
		found, err := stripBreakroomJSON(path, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "breakroom uninstall: %v\n", err)
			continue
		}
		if len(found) > 0 {
			fmt.Printf("Removed Breakroom from %s: %s\n", path, strings.Join(found, ", "))
		}
		// A hooks file left holding only its version has nothing to keep.
		if root, ok, err := readOrderedJSON(path); err == nil && ok && len(root.keys) == 1 && root.keys[0] == "version" {
			_ = os.Remove(path)
		}
	}
}
