//go:build unix

package vis

import (
	"syscall"
	"time"
)

// procCPU is the CPU time the process has used, user and system, from the OS.
// It stands in for a cgroup's usage where there is none, such as on macOS.
func procCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
