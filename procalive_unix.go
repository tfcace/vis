//go:build unix

package vis

import (
	"errors"
	"syscall"
)

// processAlive is whether process pid runs: signal 0 checks without sending.
// EPERM is a process of another user's, alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
