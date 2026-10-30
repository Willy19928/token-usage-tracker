//go:build linux

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func InstallService(executable, stateDir string) (ServiceResult, error) {
	if strings.ContainsAny(executable+stateDir, "\x00\r\n") {
		return ServiceResult{}, fmt.Errorf("服務路徑包含不安全的控制字元")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ServiceResult{}, err
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	unitDir := filepath.Join(configHome, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		return ServiceResult{}, err
	}
	unitPath := filepath.Join(unitDir, "codex-usage.service")
	unit := `[Unit]
Description=Token Usage local analytics service
After=default.target

[Service]
Type=simple
Environment=` + systemdQuote("CODEX_USAGE_HOME="+stateDir) + `
ExecStart=` + systemdQuote(executable) + ` daemon
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=default.target
`
	if err := os.WriteFile(unitPath, []byte(unit), 0o600); err != nil {
		return ServiceResult{}, err
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		if startErr := StartDetached(executable, "daemon"); startErr != nil {
			return ServiceResult{Installed: true, Warning: "無 systemd --user，且背景啟動失敗: " + startErr.Error()}, nil
		}
		return ServiceResult{
			Installed: true, Started: true,
			Warning: "未偵測到 systemctl；本次已啟動，但需自行設定登入自啟",
			Detail:  unitPath,
		}, nil
	}
	if output, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		if startErr := StartDetached(executable, "daemon"); startErr != nil {
			return ServiceResult{}, fmt.Errorf("systemctl --user daemon-reload: %w: %s；背景啟動也失敗: %v",
				err, strings.TrimSpace(string(output)), startErr)
		}
		return ServiceResult{
			Installed: true, Started: true, Detail: unitPath,
			Warning: "systemd --user 目前不可用；本次已背景啟動，但登入自啟需在 user bus 可用後執行 systemctl --user enable --now codex-usage",
		}, nil
	}
	output, err := exec.Command("systemctl", "--user", "enable", "--now", "codex-usage.service").CombinedOutput()
	if err != nil {
		if startErr := StartDetached(executable, "daemon"); startErr != nil {
			return ServiceResult{}, fmt.Errorf("systemctl --user enable --now: %w: %s；背景啟動也失敗: %v",
				err, strings.TrimSpace(string(output)), startErr)
		}
		return ServiceResult{
			Installed: true, Started: true, Detail: unitPath,
			Warning: "systemd unit 已寫入但 enable --now 失敗；本次已背景啟動，請檢查 user bus/linger 後重試",
		}, nil
	}
	result := ServiceResult{Installed: true, Started: true, Detail: unitPath}
	if user := os.Getenv("USER"); user != "" {
		if value, err := exec.Command("loginctl", "show-user", user, "-p", "Linger", "--value").Output(); err == nil &&
			strings.TrimSpace(string(value)) != "yes" {
			result.Warning = "目前使用者 linger 未開啟：退出全部登入工作階段後 systemd --user 服務可能停止；可請管理員執行 loginctl enable-linger " + user
		}
	}
	return result, nil
}

func LockDown(path string) error { return os.Chmod(path, 0o700) }

func SetPrivateUmask() { syscall.Umask(0o077) }

func UninstallService(_ string, stateDir string) error {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_, _ = exec.Command("systemctl", "--user", "disable", "--now", "codex-usage.service").CombinedOutput()
	}
	home, _ := os.UserHomeDir()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	_ = os.Remove(filepath.Join(configHome, "systemd", "user", "codex-usage.service"))
	if _, err := exec.LookPath("systemctl"); err == nil {
		_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	}
	if pid, err := ReadPID(stateDir); err == nil && pid != os.Getpid() {
		executable, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if filepath.Base(executable) == "codex-usage" {
			process, findErr := os.FindProcess(pid)
			if findErr != nil {
				return findErr
			}
			_ = process.Signal(syscall.SIGTERM)
		}
	}
	_ = os.Remove(filepath.Join(stateDir, "codex-usage.pid"))
	return nil
}

func StopPreviousService(previous PreviousService) error {
	if _, err := exec.LookPath("systemctl"); err == nil && previous.ServiceName != "" {
		_, _ = exec.Command("systemctl", "--user", "disable", "--now", previous.ServiceName).CombinedOutput()
	}
	home, _ := os.UserHomeDir()
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	if previous.ServiceName != "" {
		_ = os.Remove(filepath.Join(configHome, "systemd", "user", previous.ServiceName))
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	}
	if pid, err := readPIDFile(previous.PIDPath); err == nil && pid != os.Getpid() {
		executable, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if filepath.Base(executable) == filepath.Base(previous.Executable) {
			if process, findErr := os.FindProcess(pid); findErr == nil {
				_ = process.Signal(syscall.SIGTERM)
			}
		}
	}
	if err := os.Remove(previous.PIDPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func RemovePreviousExecutable(previous PreviousService) error {
	return removePreviousExecutable(previous)
}

func StartDetached(executable string, args ...string) error {
	command := exec.Command(executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	return command.Start()
}

func HideConsole() {}

func HasGUI() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func OpenURL(rawURL string) error {
	if !HasGUI() {
		return fmt.Errorf("無圖形環境")
	}
	return exec.Command("xdg-open", rawURL).Start()
}

func RemoveInstalledExecutable(executable, stateDir string, purge bool) error {
	exeAbs, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	stateAbs, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	if filepath.Base(exeAbs) != "codex-usage" {
		return fmt.Errorf("拒絕清理非 codex-usage 執行檔")
	}
	if purge {
		if err := ValidatePurgeStateDir(stateAbs); err != nil {
			return err
		}
	}
	if err := os.Remove(exeAbs); err != nil && !os.IsNotExist(err) {
		return err
	}
	if purge {
		return os.RemoveAll(stateAbs)
	}
	return nil
}

func systemdQuote(path string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`%`, `%%`,
	).Replace(path)
	return `"` + escaped + `"`
}
