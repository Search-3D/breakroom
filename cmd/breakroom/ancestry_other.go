//go:build !darwin

package main

import (
	"os"
	"strconv"
	"strings"
)

// osParentPID reads pid's parent from /proc (Linux), or returns 0 without
// /proc so hostAncestors falls back to ps.
func osParentPID(pid int64) int64 {
	data, err := os.ReadFile("/proc/" + strconv.FormatInt(pid, 10) + "/stat")
	if err != nil {
		return 0
	}
	// "pid (comm) state ppid ...": comm may hold spaces and parens.
	s := string(data)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.ParseInt(f[1], 10, 64)
	return ppid
}
