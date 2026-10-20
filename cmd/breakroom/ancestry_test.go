package main

import (
	"os"
	"runtime"
	"testing"
)

func TestHostAncestors(t *testing.T) {
	got := hostAncestors(t.Context())
	if len(got) == 0 || got[0] != int64(os.Getppid()) {
		t.Fatalf("ancestors = %v, want to start with ppid %d", got, os.Getppid())
	}
	if len(got) > 32 {
		t.Fatalf("len = %d, want <= 32", len(got))
	}
	seen := map[int64]bool{}
	for _, p := range got {
		if p == 1 || seen[p] {
			t.Fatalf("ancestors = %v: contains 1 or a duplicate", got)
		}
		seen[p] = true
	}
}

// The native lookup (sysctl on macOS, /proc on Linux) is what keeps hooks
// from spawning ps.
func TestOSParentPID(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no native parent lookup")
	}
	if got := osParentPID(int64(os.Getpid())); got != int64(os.Getppid()) {
		t.Fatalf("osParentPID(self) = %d, want %d", got, os.Getppid())
	}
}
