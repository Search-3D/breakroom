package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/mattn/go-isatty"
)

// isInteractiveTerminal reports whether stdin and stdout are both a terminal.
func isInteractiveTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// enterCbreak switches the terminal to cbreak mode and returns a func that restores it.
func enterCbreak() (func(), error) {
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	oldState, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("getting terminal state: %w", err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			r := exec.Command("stty", strings.TrimSpace(string(oldState)))
			r.Stdin = os.Stdin
			_ = r.Run()
		})
	}

	raw := exec.Command("stty", "cbreak", "-echo", "-isig")
	raw.Stdin = os.Stdin
	if err := raw.Run(); err != nil {
		restore()
		return nil, fmt.Errorf("setting terminal to cbreak mode: %w", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		restore()
		os.Exit(130)
	}()

	return func() {
		signal.Stop(sigCh)
		restore()
	}, nil
}

// selectMenu draws header and numbered options, then reads arrow keys, j/k,
// digits, and Enter until the user picks one. Returns the chosen index.
func selectMenu(header []string, options []string) (int, error) {
	selected := 0
	numOptions := len(options)

	render := func(first bool) {
		if !first {
			fmt.Printf("\033[%dA", len(header)+numOptions+1)
		}
		for _, h := range header {
			fmt.Println(h)
		}
		for i, opt := range options {
			cursor := "  "
			if i == selected {
				cursor = "> "
			}
			fmt.Printf("\033[2K%s[%d] %s\n", cursor, i+1, opt)
		}
		fmt.Printf("\033[2K\n")
	}

	render(true)

	buf := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 0, err
		}
		if buf[0] == '\r' || buf[0] == '\n' {
			return selected, nil
		}
		if n == 1 {
			switch buf[0] {
			case 3: // Ctrl+C
				fmt.Println("^C")
				return 0, errAborted
			case 'k', 'K':
				if selected > 0 {
					selected--
					render(false)
				}
			case 'j', 'J':
				if selected < numOptions-1 {
					selected++
					render(false)
				}
			default:
				if buf[0] >= '1' && int(buf[0]-'1') < numOptions {
					selected = int(buf[0] - '1')
					render(false)
					return selected, nil
				}
			}
		} else if n == 3 && buf[0] == 27 && buf[1] == '[' {
			switch buf[2] {
			case 'A': // Up arrow
				if selected > 0 {
					selected--
					render(false)
				}
			case 'B': // Down arrow
				if selected < numOptions-1 {
					selected++
					render(false)
				}
			}
		}
	}
}

// promptHostInteractive shows the host menu and returns the chosen host id.
func promptHostInteractive() (string, error) {
	if !isInteractiveTerminal() {
		return "", errors.New("interactive terminal prompt unavailable (pass --host)")
	}
	restore, err := enterCbreak()
	if err != nil {
		return "", err
	}
	defer restore()

	labels := make([]string, 0, len(hostTargets))
	for _, h := range hostTargets {
		labels = append(labels, fmt.Sprintf("%-26s (%s)", h.Name, h.RelativeSkillDir))
	}
	header := fmt.Sprintf("Select your agent host (Use ↑/↓ arrows or 1-%d, Enter to confirm):", len(labels))
	i, err := selectMenu([]string{header}, labels)
	if err != nil {
		return "", err
	}
	return hostTargets[i].ID, nil
}

// readLineWithInterrupt reads a line from stdin interactively, properly handling Ctrl+C,
// Backspace, and Enter without hanging or breaking terminal state.
func readLineWithInterrupt(prompt string) (string, error) {
	fmt.Print(prompt)
	if !isInteractiveTerminal() {
		return readPlainLine()
	}
	restore, err := enterCbreak()
	if err != nil {
		return readPlainLine()
	}
	defer restore()

	var input []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\r' || buf[0] == '\n' {
			fmt.Println()
			return strings.TrimSpace(string(input)), nil
		}
		if buf[0] == 3 { // Ctrl+C
			fmt.Println("^C")
			return "", errAborted
		}
		if buf[0] == 127 || buf[0] == '\b' {
			if len(input) > 0 {
				input = input[:len(input)-1]
				fmt.Print("\b \b")
			}
			continue
		}
		if buf[0] >= 32 && buf[0] != 127 {
			input = append(input, buf[0])
			_, _ = os.Stdout.Write(buf[:1])
		}
	}
}

// readPlainLine reads one line from stdin without terminal tricks, for
// pipes or when cbreak mode is unavailable.
func readPlainLine() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.Contains(line, "\x03") || (err != nil && len(line) == 0) {
		return "", errAborted
	}
	return strings.TrimSpace(line), nil
}

// projectOrGlobalPrompt labels the directory prompt for hosts that default to
// a global install: an entered path is a project, not the install target.
func projectOrGlobalPrompt(host *HostTarget) string {
	return fmt.Sprintf("Project directory (installs to %s) [blank = global: %s]: ",
		resolveTarget(host, "<dir>", "", false),
		resolveTarget(host, "", "~", true))
}

// promptDirInteractive asks where to install: a project folder, or (for
// plugin hosts) blank for the global install, which it returns as "global".
func promptDirInteractive(host *HostTarget) (string, error) {
	defaultDir := "."
	promptMsg := fmt.Sprintf("Project directory (installs to <dir>/%s) [.]: ", host.RelativeSkillDir)
	if isPluginHost(host.ID) {
		defaultDir = "global"
		promptMsg = projectOrGlobalPrompt(host)
	}
	if !isInteractiveTerminal() {
		return defaultDir, nil
	}

	line, err := readLineWithInterrupt(promptMsg)
	if err != nil {
		return "", err
	}
	if dir := strings.TrimSpace(line); dir != "" {
		return dir, nil
	}
	return defaultDir, nil
}
