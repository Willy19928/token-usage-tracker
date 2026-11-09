package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ServiceResult struct {
	Installed bool
	Started   bool
	Warning   string
	Detail    string
}

func WritePID(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "token-usage.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

func ReadPID(stateDir string) (int, error) {
	return readPIDFile(filepath.Join(stateDir, "token-usage.pid"))
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("無效 PID 檔案")
	}
	return pid, nil
}

func ValidatePurgeStateDir(stateDir string) error {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	clean := filepath.Clean(absolute)
	root := filepath.Clean(filepath.VolumeName(clean) + string(os.PathSeparator))
	if samePlatformPath(clean, root) {
		return fmt.Errorf("拒絕 purge：服務資料夾不能是檔案系統根目錄")
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && samePlatformPath(clean, filepath.Clean(home)) {
		return fmt.Errorf("拒絕 purge：服務資料夾不能是使用者主目錄")
	}
	data, err := os.ReadFile(filepath.Join(absolute, ".token-usage-state"))
	if err != nil {
		return fmt.Errorf("拒絕 purge：%s 缺少 token-usage 狀態標記", absolute)
	}
	if strings.TrimSpace(string(data)) != "token-usage-state-v1" {
		return fmt.Errorf("拒絕 purge：%s 狀態標記無效", absolute)
	}
	return nil
}

func samePlatformPath(a, b string) bool {
	if os.PathSeparator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}
