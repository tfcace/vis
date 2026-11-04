//go:build !unix

package vis

// processAlive can't tell here, so every note reads as a former process's.
func processAlive(int) bool { return false }
