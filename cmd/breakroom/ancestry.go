package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// hostAncestors returns this process's ancestor PIDs, nearest first, stopping before pid 1.
// The daemon matches them against each agent's host PID to tell which agent
// a hook belongs to. On failure it returns nil and callers match by folder.
func hostAncestors(ctx context.Context) []int64 {
	var chain []int64
	seen := map[int64]bool{}
	for p := int64(os.Getppid()); p > 1 && !seen[p] && len(chain) < 32; {
		seen[p] = true
		chain = append(chain, p)
		parent := osParentPID(p)
		if parent <= 1 || seen[parent] {
			break
		}
		p = parent
	}
	if len(chain) > 1 {
		return chain
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		if len(chain) > 0 {
			return chain
		}
		return nil
	}
	parent := map[int64]int64{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.ParseInt(f[0], 10, 64)
		ppid, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 == nil && err2 == nil {
			parent[pid] = ppid
		}
	}
	chain = nil
	seen = map[int64]bool{}
	for p := int64(os.Getppid()); p > 1 && !seen[p] && len(chain) < 32; p = parent[p] {
		seen[p] = true
		chain = append(chain, p)
	}
	return chain
}
