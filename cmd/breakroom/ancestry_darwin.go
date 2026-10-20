//go:build darwin

package main

import (
	"golang.org/x/sys/unix"
)

// osParentPID returns the parent PID of pid, or 0 if it can't be read.
func osParentPID(pid int64) int64 {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil {
		return 0
	}
	return int64(kp.Eproc.Ppid)
}
