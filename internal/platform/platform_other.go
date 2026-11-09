//go:build !windows && !linux && !darwin

package platform

import (
	"fmt"
	"os"
	"os/exec"
)

func InstallService(executable, stateDir string) (ServiceResult, error) {
	return ServiceResult{Warning: "目前平台沒有自動服務安裝器"}, nil
}
func LockDown(path string) error                       { return os.Chmod(path, 0o700) }
func SetPrivateUmask()                                 {}
func UninstallService(_ string, stateDir string) error { return nil }
func StartDetached(executable string, args ...string) error {
	return exec.Command(executable, args...).Start()
}
func HideConsole()                {}
func HasGUI() bool                { return false }
func OpenURL(rawURL string) error { return fmt.Errorf("目前平台不支援自動開啟瀏覽器") }
func RemoveInstalledExecutable(executable, stateDir string, purge bool) error {
	if purge {
		if err := ValidatePurgeStateDir(stateDir); err != nil {
			return err
		}
	}
	if err := os.Remove(executable); err != nil && !os.IsNotExist(err) {
		return err
	}
	if purge {
		return os.RemoveAll(stateDir)
	}
	return nil
}
