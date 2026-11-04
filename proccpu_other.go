//go:build !unix

package vis

import "time"

// procCPU reports nothing where vis has no way to ask the OS (see proccpu_unix.go).
func procCPU() (time.Duration, bool) { return 0, false }
