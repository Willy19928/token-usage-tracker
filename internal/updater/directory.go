package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func defaultDownloadDirectory(stateDir string) string {
	if dir, err := userDownloadsDirectory(); err == nil && filepath.IsAbs(dir) {
		return filepath.Join(dir, "token-usage")
	}
	return filepath.Join(stateDir, "downloads")
}
func (m *Manager) effectiveDownloadDir(custom string) string {
	if custom != "" {
		return custom
	}
	return m.status.DefaultDownloadDir
}
func ensureDownloadDirectory(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n\t") {
		return fmt.Errorf("下載資料夾必須是完整路徑")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("建立下載資料夾: %w", err)
	}
	f, err := os.CreateTemp(path, ".token-usage-write-check-*")
	if err != nil {
		return fmt.Errorf("下載資料夾無法寫入: %w", err)
	}
	closeErr := f.Close()
	removeErr := os.Remove(f.Name())
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
func (m *Manager) SetPreferences(autoCheck *bool, downloadDir *string) error {
	m.Status()
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.status
	if downloadDir != nil {
		if busy(m.status.Phase) {
			return fmt.Errorf("請等更新完成後再變更下載資料夾")
		}
		custom := strings.TrimSpace(*downloadDir)
		if custom != "" {
			custom = filepath.Clean(custom)
		}
		if err := ensureDownloadDirectory(m.effectiveDownloadDir(custom)); err != nil {
			return err
		}
		m.status.CustomDownloadDir = custom
		m.status.DownloadDir = m.effectiveDownloadDir(custom)
	}
	if autoCheck != nil {
		m.status.AutoCheck = *autoCheck
	}
	if err := m.save(); err != nil {
		m.status = old
		return err
	}
	return nil
}
func (m *Manager) SetDirectoryOpener(open func(string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openDirectory = open
	m.status.CanOpenDownloadDir = open != nil
}
func (m *Manager) OpenDownloadDirectory() error {
	m.mu.Lock()
	path, open := m.status.DownloadDir, m.openDirectory
	m.mu.Unlock()
	if open == nil {
		return fmt.Errorf("這裡無法開啟資料夾")
	}
	if err := ensureDownloadDirectory(path); err != nil {
		return err
	}
	return open(path)
}

// The public download folder contains only the release binary. Stage a verified
// private copy so that backup/helper files never enter a user-selected folder.
func stageDownload(source, destination, digest string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return Verify(destination, digest)
}
