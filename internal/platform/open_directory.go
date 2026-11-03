package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func OpenDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("需要完整的資料夾路徑")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("路徑不是資料夾")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", path)
	case "darwin":
		cmd = exec.Command("open", path)
	case "linux":
		if !HasGUI() {
			return fmt.Errorf("沒有桌面工作階段，請改為複製資料夾路徑")
		}
		cmd = exec.Command("xdg-open", path)
	default:
		return fmt.Errorf("這個平台無法開啟資料夾")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
