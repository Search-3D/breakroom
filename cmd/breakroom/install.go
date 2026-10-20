package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/search-3d/breakroom"
	"github.com/search-3d/breakroom/internal/hosts"
	"github.com/search-3d/breakroom/internal/version"
)

type HostTarget = hosts.Target

var errAborted = errors.New("aborted by user")

var hostTargets = hosts.InstallTargets()

// findHostTarget looks up a host by id. "copilot" means the VS Code plugin.
func findHostTarget(id string) *HostTarget {
	if strings.EqualFold(strings.TrimSpace(id), "copilot") {
		return hosts.Find("vscode")
	}
	return hosts.Find(id)
}

// installOpts is the resolved install request handed to each host installer.
type installOpts struct {
	host    *HostTarget
	target  string
	baseDir string
	home    string
	global  bool
	binPath string
}

// isPluginHost reports whether the host installs a plugin, which defaults
// to the user's global scope.
func isPluginHost(id string) bool {
	switch id {
	case "antigravity", "cursor", "vscode", "claude", "codex", "muse", "opencode":
		return true
	default:
		return false
	}
}

// skillDocs are the skill files relative to the skill directory itself.
func skillDocs() map[string][]byte {
	return map[string][]byte{
		"SKILL.md": breakroom.SkillMD,
		filepath.Join("docs", "tool-contracts.md"): breakroom.ToolContractsMD,
		filepath.Join("docs", "blueprint-spec.md"): breakroom.BlueprintSpecMD,
	}
}

// skillFiles are the skill files relative to a plugin root.
func skillFiles() map[string][]byte {
	files := make(map[string][]byte, 3)
	for rel, content := range skillDocs() {
		files[filepath.Join("skills", "breakroom", rel)] = content
	}
	return files
}

var skillDocList = []string{"SKILL.md", "docs/tool-contracts.md", "docs/blueprint-spec.md"}

// skillFileList is skillDocList as paths inside a plugin root, for printing.
func skillFileList() []string {
	list := make([]string, 0, len(skillDocList))
	for _, f := range skillDocList {
		list = append(list, "skills/breakroom/"+f)
	}
	return list
}

// commandRe matches the first quoted word of a "command" value in JSON or
// TypeScript, whether a plain string or the start of an argv array.
var commandRe = regexp.MustCompile(`command"?\s*:\s*\[?\s*"([^"]+)"`)

// detectExistingBinary finds the absolute breakroom path an earlier install
// in target was configured with, so a reinstall keeps it. "" if none.
func detectExistingBinary(target string) string {
	candidateFiles := []string{
		"mcp.json",
		"mcp_config.json",
		".mcp.json",
		filepath.Join(".muse-plugin", "plugin.json"),
		filepath.Join("plugins", "breakroom.ts"),
	}
	for _, rel := range candidateFiles {
		data, err := os.ReadFile(filepath.Join(target, rel))
		if err != nil {
			continue
		}
		for _, m := range commandRe.FindAllStringSubmatch(string(data), -1) {
			if cmd := m[1]; filepath.IsAbs(cmd) && filepath.Base(cmd) == "breakroom" {
				return cmd
			}
		}
	}
	return ""
}

// adjustBinaryPath points the configs' breakroom commands at bin, both the
// argv form (["breakroom", ...]) and the shell-string form ("breakroom gate ...").
func adjustBinaryPath(content []byte, bin string) []byte {
	if bin == "" || bin == "breakroom" {
		return content
	}
	s := string(content)
	s = strings.ReplaceAll(s, `"command": "breakroom"`, `"command": `+jsonString(bin))
	s = strings.ReplaceAll(s, `["breakroom",`, `[`+jsonString(bin)+`,`)
	line := jsonString(shellQuote(bin) + " ")
	s = strings.ReplaceAll(s, `"command": "breakroom `, `"command": `+line[:len(line)-1])
	return []byte(s)
}

// jsonString returns s as a quoted JSON (and JavaScript) string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// shellQuote single-quotes s when a shell would split or expand it.
func shellQuote(s string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%", r)
	}
	if strings.IndexFunc(s, func(r rune) bool { return !safe(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeFiles writes files under target, pointing commands at the breakroom
// binary, and records the installed version in .breakroom-version.
func writeFiles(target string, files map[string][]byte, binPath string) error {
	resolvedBin := binPath
	if resolvedBin == "" {
		resolvedBin = detectExistingBinary(target)
	}
	for relPath, content := range files {
		fullPath := filepath.Join(target, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", relPath, err)
		}
		if resolvedBin != "" && (strings.HasSuffix(relPath, ".json") || strings.HasSuffix(relPath, ".ts")) {
			content = adjustBinaryPath(content, resolvedBin)
		}
		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", relPath, err)
		}
	}
	return os.WriteFile(filepath.Join(target, ".breakroom-version"), []byte(version.Version+"\n"), 0o644)
}

// printCopied lists the files an install wrote into dir.
func printCopied(dir string, files ...string) {
	fmt.Println()
	fmt.Printf("Copied to %s:\n", dir)
	for _, f := range files {
		fmt.Println("- " + f)
	}
}

// runInstall is `breakroom install`: it picks the host and scope (asking
// when run interactively) and hands off to that host's installer.
func runInstall(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "agent host: "+hosts.InstallHostsString())
	globalFlag := fs.Bool("global", false, "install globally for the host (default for every host except generic)")
	binPathFlag := fs.String("bin-path", "", "path to breakroom binary to use in configs (default: detect from existing configs, or breakroom)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	interactive := false
	hostID := *hostFlag
	if hostID == "" {
		chosen, err := promptHostInteractive()
		if err != nil {
			if errors.Is(err, errAborted) {
				return 130
			}
			fmt.Fprintf(os.Stderr, "breakroom install: %v\n\nUsage: breakroom install [--host <host>] [--global] [DIR]\nValid hosts: %s\n", err, hosts.InstallHostsString())
			return 2
		}
		hostID = chosen
		interactive = true
	}

	targetHost := findHostTarget(hostID)
	if targetHost == nil {
		fmt.Fprintf(os.Stderr, "breakroom install: unknown host %q\nValid hosts: %s\n", hostID, hosts.InstallHostsString())
		return 2
	}

	isGlobal := *globalFlag
	baseDir := ""
	switch {
	case fs.NArg() > 0:
		baseDir = fs.Arg(0)
	case interactive:
		chosenDir, err := promptDirInteractive(targetHost)
		if err != nil {
			if errors.Is(err, errAborted) {
				return 130
			}
			fmt.Fprintf(os.Stderr, "breakroom install: %v\n", err)
			return 1
		}
		if chosenDir == "global" {
			isGlobal = true
		} else {
			baseDir = chosenDir
		}
	case isPluginHost(targetHost.ID):
		isGlobal = true
	default:
		baseDir = "."
	}

	home, err := os.UserHomeDir()
	if isGlobal && (err != nil || home == "") {
		fmt.Fprintf(os.Stderr, "breakroom install: cannot find home directory: %v\n", err)
		return 1
	}

	baseDir = expandHome(baseDir, home)
	o := installOpts{
		host:    targetHost,
		target:  resolveTarget(targetHost, baseDir, home, isGlobal),
		baseDir: baseDir,
		home:    home,
		global:  isGlobal,
		binPath: *binPathFlag,
	}

	switch targetHost.ID {
	case "antigravity":
		err = installAntigravity(o)
	case "cursor":
		err = installCursor(o)
	case "claude":
		err = installClaude(o)
	case "codex":
		err = installCodex(o)
	case "opencode":
		err = installOpenCode(o)
	case "vscode":
		err = installVSCode(o)
	case "muse":
		err = installMuse(o)
	default:
		err = installGeneric(o)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "breakroom install: %v\n", err)
		return 1
	}
	return 0
}

// expandHome replaces a leading ~ in dir with the home directory.
func expandHome(dir, home string) string {
	if home == "" {
		return dir
	}
	if dir == "~" {
		return home
	}
	if strings.HasPrefix(dir, "~/") {
		return filepath.Join(home, dir[2:])
	}
	return dir
}

// projectRel is where a host's files go inside a project.
func projectRel(host *HostTarget) string {
	if host.ID == "cursor" {
		// Cursor loads plugins only globally; a project gets a skill plus mcp/hooks config.
		return filepath.Join(".cursor", "skills", "breakroom")
	}
	return host.RelativeSkillDir
}

// projectRoot is the project folder holding a project-scope target.
func projectRoot(host *HostTarget, target string) string {
	root := strings.TrimSuffix(filepath.Clean(target), projectRel(host))
	if root == "" {
		return "."
	}
	return filepath.Clean(root)
}

// resolveTarget returns the directory the host's plugin or skill files go into.
func resolveTarget(host *HostTarget, baseDir, home string, global bool) string {
	if global {
		switch host.ID {
		case "antigravity":
			return filepath.Join(home, ".gemini", "config", "plugins", "breakroom")
		case "vscode":
			return filepath.Join(home, ".copilot", "installed-plugins", "_direct", "breakroom")
		case "claude":
			return filepath.Join(home, ".claude", "skills", "breakroom")
		case "codex":
			return filepath.Join(home, ".agents", "plugins", "breakroom")
		case "opencode":
			return filepath.Join(home, ".config", "opencode", "skills", "breakroom")
		case "muse":
			return filepath.Join(home, ".config", "muse", "plugins", "breakroom")
		default:
			return filepath.Join(home, host.RelativeSkillDir)
		}
	}
	rel := projectRel(host)
	if cleanBase := filepath.Clean(baseDir); strings.HasSuffix(cleanBase, rel) {
		return cleanBase
	}
	return filepath.Join(baseDir, rel)
}

// versionDir is where an install of host at target keeps .breakroom-version.
// OpenCode's target is its skill folder, two levels below the files' root.
func versionDir(host *HostTarget, target string) string {
	if host.ID == "opencode" {
		return filepath.Dir(filepath.Dir(target))
	}
	return target
}

// checkHostPlugins reports, for `breakroom doctor`, each host with a global
// install and whether it matches this binary's version.
func checkHostPlugins(home string) []hostDoctorInfo {
	var list []hostDoctorInfo
	if home == "" {
		return list
	}
	for _, t := range hosts.InstallTargets() {
		dir := resolveTarget(&t, "", home, true)
		info := hostDoctorInfo{Host: t.ID, Target: dir}
		if data, err := os.ReadFile(filepath.Join(versionDir(&t, dir), ".breakroom-version")); err == nil {
			info.Version = strings.TrimSpace(string(data))
			if info.Version == version.Version {
				info.Status = "installed (up to date)"
			} else {
				info.Status = fmt.Sprintf("stale (plugin %s, binary %s; run `breakroom install --host %s`)", info.Version, version.Version, t.ID)
			}
		} else if fileExists(filepath.Join(dir, "SKILL.md")) || fileExists(filepath.Join(dir, "skills", "breakroom", "SKILL.md")) {
			info.Status = fmt.Sprintf("installed (unversioned; run `breakroom install --host %s` to update)", t.ID)
		} else {
			continue
		}
		list = append(list, info)
	}
	return list
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runUninstall is `breakroom uninstall`: it removes one host's install, or
// every host's with --host all.
func runUninstall(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "agent host: "+hosts.InstallHostsString()+" or all")
	globalFlag := fs.Bool("global", false, "uninstall from global location")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	hostID := *hostFlag
	if hostID == "" {
		fmt.Fprintf(os.Stderr, "usage: breakroom uninstall --host <host> [--global] [DIR]\nValid hosts: %s, all\n", hosts.InstallHostsString())
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(os.Stderr, "breakroom uninstall: cannot find home directory: %v\n", err)
		return 1
	}

	targets := hostTargets
	if hostID != "all" {
		t := findHostTarget(hostID)
		if t == nil {
			fmt.Fprintf(os.Stderr, "breakroom uninstall: unknown host %q\nValid hosts: %s, all\n", hostID, hosts.InstallHostsString())
			return 2
		}
		targets = []HostTarget{*t}
	}

	baseDir := ""
	if fs.NArg() > 0 {
		baseDir = expandHome(fs.Arg(0), home)
	}

	for _, t := range targets {
		// The generic skill defaults to the current folder; "all" alone
		// means the global installs, so it must not delete it.
		if hostID == "all" && t.ID == "generic" && baseDir == "" && !*globalFlag {
			continue
		}
		uninstallOne(t, baseDir, home, *globalFlag, hostID == "all")
	}
	return 0
}

// sharedPluginFiles: Codex and Antigravity both install a project plugin
// into .agents/plugins/breakroom. Uninstalling one removes only its own
// files while the other's marker (first entry) is still there.
var sharedPluginFiles = map[string][]string{
	"antigravity": {"mcp_config.json", "plugin.json", "hooks.json"},
	"codex":       {".codex-plugin", ".mcp.json", "hooks"},
}

// uninstallOne removes one host's install, plus the config it wrote outside
// the install folder (hook files, settings entries, marketplace entries).
func uninstallOne(t HostTarget, baseDir, home string, global, all bool) {
	isGlobal := (global || isPluginHost(t.ID)) && baseDir == ""
	targetDir := resolveTarget(&t, baseDir, home, isGlobal)
	root := home
	if !isGlobal {
		root = projectRoot(&t, targetDir)
	}
	switch t.ID {
	case "claude":
		// Claude records its approval of the plugin's MCP server in settings.local.json.
		local := filepath.Join(root, ".claude", "settings.local.json")
		if removed, err := stripClaudeLocalSettings(local); err != nil {
			fmt.Fprintf(os.Stderr, "breakroom uninstall: %v\n", err)
		} else if removed {
			fmt.Printf("Removed Breakroom from %s\n", local)
		}
	case "cursor":
		if !isGlobal {
			removeCursorProject(filepath.Join(root, ".cursor"))
		}
	case "antigravity":
		if isGlobal {
			cleanAntigravityGlobal(home)
		}
	case "codex":
		removeCodexMarketplace(filepath.Join(filepath.Dir(targetDir), "marketplace.json"))
		// Codex records the enabled plugin, its trusted hooks, and any
		// manual MCP server in config.toml; none go away with the plugin.
		cfg := filepath.Join(root, ".codex", "config.toml")
		if found, err := stripCodexConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "breakroom uninstall: %v\n", err)
		} else if len(found) > 0 {
			fmt.Printf("Removed Breakroom from %s: %s\n", cfg, strings.Join(found, ", "))
		}
	case "opencode":
		// The plugin and version file sit beside skills/.
		dir := versionDir(&t, targetDir)
		_ = os.Remove(filepath.Join(dir, "plugins", "breakroom.ts"))
		_ = os.Remove(filepath.Join(dir, ".breakroom-version"))
	case "vscode":
		// The Copilot harness hook file lives outside the plugin.
		hooksDir := filepath.Join(home, ".copilot", "hooks")
		if !isGlobal {
			hooksDir = filepath.Join(root, ".github", "hooks")
		}
		_ = os.Remove(filepath.Join(hooksDir, "breakroom-copilot.json"))
		if !isGlobal {
			settings := filepath.Join(root, ".vscode", "settings.json")
			if _, err := updateVSCodePluginSettings(settings, vscodePluginKey(&t), false); err != nil {
				fmt.Fprintf(os.Stderr, "breakroom uninstall: %v\n", err)
			}
		}
	case "muse":
		// A manual setup (MCP server and hooks in the user settings, or
		// project hooks) outlives the plugin unless removed here.
		cfg := museManualConfig(isGlobal, home, root)
		if found, err := stripBreakroomJSON(cfg, true); err != nil {
			fmt.Fprintf(os.Stderr, "breakroom uninstall: %v\n", err)
		} else if len(found) > 0 {
			fmt.Printf("Removed Breakroom from %s: %s\n", cfg, strings.Join(found, ", "))
		}
	}
	if _, err := os.Stat(targetDir); err != nil {
		if !all {
			fmt.Printf("Breakroom is not installed for %s at %s\n", t.Name, targetDir)
		}
		return
	}
	if own, ok := sharedPluginFiles[t.ID]; ok {
		for other, files := range sharedPluginFiles {
			if other == t.ID || !fileExists(filepath.Join(targetDir, files[0])) {
				continue
			}
			for _, f := range own {
				_ = os.RemoveAll(filepath.Join(targetDir, f))
			}
			fmt.Printf("Uninstalled Breakroom from %s (%s; kept the files %s shares)\n", t.Name, targetDir, other)
			return
		}
	}
	if err := os.RemoveAll(targetDir); err != nil {
		fmt.Fprintf(os.Stderr, "breakroom uninstall: failed to remove %s: %v\n", targetDir, err)
		return
	}
	fmt.Printf("Uninstalled Breakroom from %s (%s)\n", t.Name, targetDir)
	if t.ID == "muse" {
		fmt.Println("Also remove it from Muse: muse plugins remove breakroom")
	}
}

// removeCodexMarketplace drops the breakroom entry from a Codex marketplace
// file, and deletes the file when it is left as the empty catalog install made.
func removeCodexMarketplace(marketplacePath string) {
	data, err := os.ReadFile(marketplacePath)
	if err != nil {
		return
	}
	withoutBreakroom := func(list []any) []any {
		kept := []any{}
		for _, p := range list {
			if pm, ok := p.(map[string]any); !ok || pm["name"] != "breakroom" {
				kept = append(kept, p)
			}
		}
		return kept
	}
	var catalog any
	var rawObj map[string]any
	var rawArr []any
	switch {
	case json.Unmarshal(data, &rawObj) == nil && rawObj != nil:
		list, ok := rawObj["plugins"].([]any)
		if !ok {
			return
		}
		rawObj["plugins"] = withoutBreakroom(list)
		if len(rawObj["plugins"].([]any)) == 0 && len(rawObj) == 2 && rawObj["name"] == "local" {
			_ = os.Remove(marketplacePath)
			return
		}
		catalog = rawObj
	case json.Unmarshal(data, &rawArr) == nil:
		catalog = withoutBreakroom(rawArr)
	default:
		return
	}
	if out, err := json.MarshalIndent(catalog, "", "  "); err == nil {
		_ = os.WriteFile(marketplacePath, append(out, '\n'), 0o644)
	}
}
