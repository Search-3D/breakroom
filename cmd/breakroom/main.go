// Command breakroom is the Breakroom multi-agent coordination broker.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/search-3d/breakroom/internal/daemon"
	"github.com/search-3d/breakroom/internal/paths"
	"github.com/search-3d/breakroom/internal/proxy"
	"github.com/search-3d/breakroom/internal/render"
	"github.com/search-3d/breakroom/internal/version"
)

type command struct {
	name  string
	usage string
	run   func(ctx context.Context, args []string) int
}

// envelope is the CLI JSON shape from docs/tool-contracts.md.
type envelope struct {
	OK       bool     `json:"ok"`
	Data     any      `json:"data"`
	Err      string   `json:"error"`
	Unread   []string `json:"_unread_messages"`
	Warnings []string `json:"_warnings"`
}

// printJSON prints a successful CLI JSON envelope.
func printJSON(data any) {
	_ = json.NewEncoder(os.Stdout).Encode(envelope{
		OK: true, Data: data, Unread: []string{}, Warnings: []string{},
	})
}

// printJSONError prints a failed CLI JSON envelope with an error code.
func printJSONError(code, detail string) {
	_ = json.NewEncoder(os.Stdout).Encode(envelope{
		Data: map[string]any{}, Err: code + ": " + detail,
		Unread: []string{}, Warnings: []string{},
	})
}

var commands = []command{
	{
		name:  "start",
		usage: "start the background daemon",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("start", flag.ContinueOnError)
			idle := fs.Duration("idle-timeout", 30*time.Minute, "shut down after this long idle with no active agents")
			foreground := fs.Bool("foreground", false, "run in-process (used to spawn the daemon)")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			if *foreground {
				if err := daemon.RunForeground(*idle); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
				return 0
			}
			pid, err := daemon.Start(*idle)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			fmt.Printf("breakroom started (pid %d)\n", pid)
			return 0
		},
	},
	{
		name:  "stop",
		usage: "stop the background daemon",
		run: func(ctx context.Context, args []string) int {
			if err := flag.NewFlagSet("stop", flag.ContinueOnError).Parse(args); err != nil {
				return 2
			}
			running, err := daemon.Stop()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			if running {
				fmt.Println("breakroom stopped")
			} else {
				fmt.Println("breakroom: not running")
			}
			return 0
		},
	},
	{
		name:  "prune",
		usage: "clear state from previous runs (messages, blueprints, logs)",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("prune", flag.ContinueOnError)
			force := fs.Bool("force", false, "prune even with agents connected")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			if info, err := daemon.QueryStatus(); err == nil && info.AgentsPresent > 0 && !*force {
				fmt.Fprintf(os.Stderr, "breakroom: %d agent(s) connected; their names and inboxes would be lost (pass --force)\n", info.AgentsPresent)
				return 1
			}
			if _, err := daemon.Stop(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			// config.json and token survive: settings, not state.
			traces, _ := filepath.Glob(filepath.Join(paths.Dir(), "trace-*.log"))
			for _, f := range append([]string{paths.DB(), paths.DB() + "-wal", paths.DB() + "-shm", paths.Log(), paths.Log() + ".1"}, traces...) {
				if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
					fmt.Fprintln(os.Stderr, "breakroom:", err)
					return 1
				}
			}
			fmt.Println("breakroom: state pruned")
			return 0
		},
	},
	{
		name:  "status",
		usage: "show daemon and agent status",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("status", flag.ContinueOnError)
			format := fs.String("format", "text", "output format: text or json")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			info, err := daemon.QueryStatus()
			if err != nil {
				if errors.Is(err, daemon.ErrNotRunning) {
					if *format == "json" {
						printJSONError("not_running", "daemon is not running")
					} else {
						fmt.Fprintln(os.Stderr, "breakroom: not running")
					}
					return 1
				}
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			if *format == "json" {
				printJSON(info)
				return 0
			}
			if info.Version != version.Version {
				fmt.Fprintf(os.Stderr, "breakroom: daemon is %s but this binary is %s; run `breakroom stop` to restart it\n", info.Version, version.Version)
			}
			present := strconv.Itoa(info.AgentsPresent)
			if info.AgentsDone > 0 {
				present += fmt.Sprintf(" (%d completed)", info.AgentsDone)
			}
			fmt.Printf("breakroom %s (pid %d, up %s)\nagents present: %s\nsocket: %s\n",
				info.Version, info.PID,
				(time.Duration(info.UptimeSec) * time.Second).Truncate(time.Second),
				present, info.Socket)
			if info.TCPAddr != "" {
				fmt.Printf("tcp: %s\n", info.TCPAddr)
			}
			printAgents(info.Agents)
			return 0
		},
	},
	{
		name:  "logs",
		usage: "show the daemon log",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("logs", flag.ContinueOnError)
			tail := fs.Int("tail", 50, "show only the last N lines")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			data, err := os.ReadFile(paths.Log())
			if err != nil {
				fmt.Fprintf(os.Stderr, "breakroom: no log yet (%s)\n", paths.Log())
				return 1
			}
			text := strings.TrimRight(string(data), "\n")
			if text == "" {
				return 0
			}
			lines := strings.Split(text, "\n")
			if *tail >= 0 && *tail < len(lines) {
				lines = lines[len(lines)-*tail:]
			}
			fmt.Println(strings.Join(lines, "\n"))
			return 0
		},
	},
	{
		name:  "doctor",
		usage: "check the local environment",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
			format := fs.String("format", "text", "output format: text or json")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			report := doctorReport(ctx)
			if *format == "json" {
				printJSON(report)
				return 0
			}
			fmt.Printf("breakroom %s\nhome:   %s\ndaemon: %s\nsocket: %s\ngit:    %s\nconfig: %s\n",
				report.Version, report.Home, report.Daemon, report.Socket, report.Git, report.Config)
			if len(report.Plugins) > 0 {
				fmt.Println("\ninstalled host plugins:")
				for _, p := range report.Plugins {
					fmt.Printf("  %-12s %s (%s)\n", p.Host+":", p.Target, p.Status)
				}
			}
			return 0
		},
	},
	{
		name:  "mcp",
		usage: "run the MCP stdio proxy (spawned by agent hosts)",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
			// Some hosts (Cursor, others) start servers in $HOME or /, but
			// the workspace and relative worktree paths come from the cwd.
			dir := fs.String("dir", "", "project directory to run in (for hosts that start servers elsewhere)")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			// Unexpanded template variables from host configs (e.g.
			// ${workspaceFolder}) are ignored.
			templated := strings.HasPrefix(*dir, "${") && strings.HasSuffix(*dir, "}")
			if *dir != "" && !templated {
				if err := os.Chdir(*dir); err != nil {
					fmt.Fprintf(os.Stderr, "breakroom: --dir: %v\n", err)
					return 1
				}
			}
			if err := proxy.Run(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			return 0
		},
	},
	{
		name:  "check",
		usage: "check for unread messages",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("check", flag.ContinueOnError)
			hook := fs.Bool("hook", false, "hook mode: print unread messages, print nothing when empty, never fail")
			hookEvent := fs.String("hook-event", "", "emit Claude Code hook JSON (additionalContext) for this event, e.g. PostToolUse; implies --hook")
			format := fs.String("format", "text", "output format: text or json")
			agent := fs.String("agent", "", "agent id (default: the agent whose worktree holds the cwd, else the sole active agent)")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			return runCheck(ctx, *hook || *hookEvent != "", *hookEvent, *format, *agent)
		},
	},
	{
		name:  "send",
		usage: "send a message to an agent",
		run: func(ctx context.Context, args []string) int {
			fs := flag.NewFlagSet("send", flag.ContinueOnError)
			to := fs.String("to", "", "recipient agent id or *")
			message := fs.String("message", "", "message body (reads stdin when empty)")
			msgType := fs.String("type", "chat", "message type")
			from := fs.String("from", "", "sender id (defaults to the sole active agent)")
			var diffPaths stringList
			fs.Var(&diffPaths, "diff-path", "attach sender diff for this path (repeatable)")
			await := fs.Bool("await", false, "block for a reply")
			timeout := fs.Int("timeout", 300, "max block in seconds when awaiting")
			if err := fs.Parse(args); err != nil {
				return 2
			}
			return runSend(ctx, *to, *message, *msgType, *from, diffPaths, *await, *timeout)
		},
	},
	{
		name:  "diff",
		usage: "show an agent's worktree diff",
		run:   diffCommand,
	},
	{
		name:  "gate",
		usage: "host hook: hold or block a stop, record human prompts",
		run:   gateCommand,
	},
	{
		name:  "blueprint",
		usage: "approve a roadmap, accept/reject finished work, record landing (blueprint approve|accept|reject|land <name>)",
		run:   blueprintCommand,
	},
	{
		name:  "compact",
		usage: "condense recent messages into a handoff digest",
		run:   compactCommand,
	},
	{
		name:  "install",
		usage: "install native plugins, skills, MCP configs, and hooks for agent hosts (install [--host <name>] [--global] [DIR])",
		run:   runInstall,
	},
	{
		name:  "uninstall",
		usage: "uninstall native plugins, skills, MCP configs, and hooks from an agent host (uninstall --host <name> [--global] [DIR])",
		run:   runUninstall,
	},
	{
		name:  "version",
		usage: "print the version",
		run: func(ctx context.Context, args []string) int {
			if err := flag.NewFlagSet("version", flag.ContinueOnError).Parse(args); err != nil {
				return 2
			}
			fmt.Println(version.Version)
			return 0
		},
	},
}

// runCheck prints unread mail for the agent working here, then marks it read.
// In hook mode it never fails, and with hookEvent it prints the host's hook JSON.
func runCheck(ctx context.Context, hook bool, hookEvent, format, agent string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return checkErr(hook, format, "internal", err.Error())
	}
	// Peek, print, then ack: a hook killed before its output is out
	// redelivers next time instead of losing the mail.
	c, ws := proxy.NewClient(), paths.ResolveWorkspace(cwd)
	msgs, resolved, err := c.Peek(ctx, ws, agent, cwd, hostAncestors(ctx))
	if err != nil {
		var perr *proxy.Error
		if errors.As(err, &perr) {
			return checkErr(hook, format, perr.Code, perr.Detail)
		}
		return checkErr(hook, format, "internal", err.Error())
	}
	if len(msgs) == 0 {
		if format == "json" && hookEvent == "" {
			printJSON(map[string]any{"messages": msgs, "recipient": resolved})
		}
		return 0
	}
	ack := func() {
		ids := make([]int64, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
		_ = c.Ack(ctx, ws, resolved, ids)
	}
	if hookEvent != "" {
		// Most hook events (e.g. Claude Code PostToolUse) drop plain stdout
		// instead of showing it to the model, so mail must go out as
		// additionalContext or acking it would lose it.
		var b strings.Builder
		fmt.Fprintf(&b, "Breakroom: %d new message(s) for %s.\n\n", len(msgs), resolved)
		for _, m := range msgs {
			b.WriteString(render.FormatMessage(m))
		}
		var out struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		out.HookSpecificOutput.HookEventName = hookEvent
		out.HookSpecificOutput.AdditionalContext = b.String()
		if json.NewEncoder(os.Stdout).Encode(out) == nil {
			ack()
		}
		return 0
	}
	if format == "json" {
		printJSON(map[string]any{"messages": msgs, "recipient": resolved})
		ack()
		return 0
	}
	for _, m := range msgs {
		fmt.Print(render.FormatMessage(m))
	}
	ack()
	return 0
}

// checkErr never fails the host in hook mode: no daemon, no identity, or no
// mail all mean silent stdout and exit 0.
func checkErr(hook bool, format, code, detail string) int {
	if hook {
		return 0
	}
	if format == "json" {
		printJSONError(code, detail)
	} else {
		fmt.Fprintf(os.Stderr, "breakroom: %s: %s\n", code, detail)
	}
	return 1
}

// runSend sends a message as an agent, optionally waiting for the reply
// until timeout seconds pass.
func runSend(ctx context.Context, to, message, msgType, from string, diffPaths []string, await bool, timeout int) int {
	if to == "" {
		fmt.Fprintln(os.Stderr, "breakroom: --to is required")
		return 2
	}
	content := message
	if content == "" {
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "breakroom: reading stdin: %v\n", err)
			return 1
		}
		content = strings.TrimRight(string(body), "\n")
	}
	ws, err := paths.CurrentWorkspace()
	if err != nil {
		return cliErr(err)
	}
	client := proxy.NewClient()
	if from == "" {
		agents, err := client.ListAgents(ctx, ws, "", false)
		if err != nil {
			return cliErr(err)
		}
		if len(agents) != 1 {
			fmt.Fprintf(os.Stderr, "breakroom: pass --from (%d agents active)\n", len(agents))
			return 1
		}
		from = agents[0].AgentID
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	resp, err := client.Send(ctx, proxy.SendRequest{
		Workspace: ws, From: from, To: to,
		Type: msgType, Content: content, AwaitReply: await, TimeoutSec: timeout,
		IncludeDiffPaths: diffPaths,
	})
	if err != nil {
		return cliErr(err)
	}
	// The broker holds each request at most wait_hold_max_sec; no host times
	// the CLI out, so keep re-waiting for the reply until --timeout.
	for await && resp.TimedOut && len(resp.MessageIDs) == 1 {
		secs := int(time.Until(deadline).Seconds())
		if secs < 1 {
			break
		}
		if resp.Reply, resp.TimedOut, err = client.AwaitReply(ctx, ws, from, to, resp.MessageIDs[0], secs); err != nil {
			return cliErr(err)
		}
	}
	if len(resp.MessageIDs) == 1 {
		fmt.Printf("sent message %d\n", resp.MessageIDs[0])
	} else {
		fmt.Printf("sent %d messages %v\n", len(resp.MessageIDs), resp.MessageIDs)
	}
	if await {
		if resp.TimedOut {
			fmt.Println("no reply (timed out)")
		} else {
			fmt.Print(render.FormatMessage(*resp.Reply))
		}
	}
	return 0
}

// cliErr prints err for a human and returns exit code 1.
func cliErr(err error) int {
	var perr *proxy.Error
	if errors.As(err, &perr) {
		fmt.Fprintf(os.Stderr, "breakroom: %s: %s\n", perr.Code, perr.Detail)
	} else {
		fmt.Fprintf(os.Stderr, "breakroom: %v\n", err)
	}
	return 1
}

type hostDoctorInfo struct {
	Host    string `json:"host"`
	Target  string `json:"target"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

type doctorInfo struct {
	Version string           `json:"version"`
	Home    string           `json:"home"`
	Daemon  string           `json:"daemon"`
	Socket  string           `json:"socket"`
	Git     string           `json:"git"`
	Config  string           `json:"config"`
	Plugins []hostDoctorInfo `json:"plugins"`
}

// doctorReport gathers what `breakroom doctor` shows: daemon, socket, git,
// config, and installed host plugins.
func doctorReport(ctx context.Context) doctorInfo {
	r := doctorInfo{Version: version.Version, Home: paths.Dir()}
	switch info, err := daemon.QueryStatus(); {
	case err == nil:
		r.Daemon = fmt.Sprintf("running (pid %d)", info.PID)
		if info.Version != version.Version {
			r.Daemon += fmt.Sprintf(" - STALE: daemon %s, this binary %s; run `breakroom stop` (agents reconnect automatically)", info.Version, version.Version)
		}
	case errors.Is(err, daemon.ErrNotRunning):
		r.Daemon = "not running"
	default:
		r.Daemon = "unknown (" + err.Error() + ")"
	}
	r.Socket = paths.Socket()
	if _, err := os.Stat(paths.Socket()); err == nil {
		r.Socket += " (exists)"
	}
	if out, err := exec.CommandContext(ctx, "git", "--version").Output(); err == nil {
		r.Git = strings.TrimSpace(string(out))
	} else {
		r.Git = "missing"
	}
	r.Config = "missing"
	if _, err := os.Stat(paths.Config()); err == nil {
		r.Config = paths.Config()
	}
	home, _ := os.UserHomeDir()
	r.Plugins = checkHostPlugins(home)
	return r
}

// usage prints the list of commands.
func usage() {
	fmt.Fprint(os.Stderr, "Usage: breakroom <command> [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", c.name, c.usage)
	}
	fmt.Fprint(os.Stderr, "\nRun 'breakroom <command> -h' for command help.\n")
}

// run dispatches args to the matching command and returns its exit code.
func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	if name := args[0]; name == "help" || name == "-h" || name == "-help" || name == "--help" {
		usage()
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}
	fmt.Fprintf(os.Stderr, "breakroom: unknown command %q\n\n", args[0])
	usage()
	return 2
}

// main runs the CLI, cancelling the context on Ctrl+C or SIGTERM.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // Unregister on first signal so a subsequent signal invokes default OS termination.
	}()
	code := run(ctx, os.Args[1:])
	os.Exit(code)
}

// printAgents lists connected agents under their workspace.
func printAgents(agents []daemon.AgentInfo) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	ws := ""
	for _, a := range agents {
		if a.WorkspaceRoot != ws {
			ws = a.WorkspaceRoot
			_ = tw.Flush()
			fmt.Printf("\n%s\n", ws)
		}
		role := a.Role
		if role == "" {
			role = "-"
		}
		where := a.WorktreePath
		if rel, err := filepath.Rel(ws, where); err == nil && len(rel) < len(where) {
			where = rel
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", a.AgentID, role, a.Status, where)
	}
	_ = tw.Flush()
}
