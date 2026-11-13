//go:build linux || darwin

package platform

import (
	"os"
	"syscall"
)

// ProcessRunsExecutable reports whether process pid is alive. The executable
// is not compared here; a live PID from the service folder's PID file is
// treated as the service.
func ProcessRunsExecutable(pid int, _ ...string) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
