//go:build !windows && !linux && !darwin

package platform

func ProcessRunsExecutable(pid int, _ ...string) bool { return false }
