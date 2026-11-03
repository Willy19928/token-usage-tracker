//go:build !windows && !linux && !darwin

package platform

import "fmt"

func ManagedUpdateService() bool { return false }
func StartUpdateHelper(executable, job, stateDir string, managed bool) error {
	return fmt.Errorf("這個平台不支援自動更新")
}
func StopForUpdate(executable, stateDir string, managed bool) error {
	return fmt.Errorf("這個平台不支援自動更新")
}
func StartAfterUpdate(executable string, managed bool) error {
	return fmt.Errorf("這個平台不支援自動更新")
}
