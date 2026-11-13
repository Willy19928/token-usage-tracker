//go:build windows

package platform

import (
	"path/filepath"
	"strings"
)

// ProcessRunsExecutable reports whether process pid is alive and runs one of
// the given executables.
func ProcessRunsExecutable(pid int, executables ...string) bool {
	if pid <= 0 {
		return false
	}
	path, err := windowsProcessExecutable(uint32(pid))
	if err != nil {
		return false
	}
	for _, executable := range executables {
		if executable != "" && strings.EqualFold(filepath.Clean(path), filepath.Clean(executable)) {
			return true
		}
	}
	return false
}
