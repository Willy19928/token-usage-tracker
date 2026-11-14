package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// defaultDownloadDirectory keeps update downloads out of the folders people
// use. A download there is removed once the installer has been staged; only a
// folder the user chose keeps its copy.
func defaultDownloadDirectory(stateDir string) string {
	if dir := os.TempDir(); filepath.IsAbs(dir) {
		return filepath.Join(dir, "token-usage-updates")
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

// removeFinishedRuns deletes the working folders of earlier updates, with the
// backups they kept, before a new update starts. A folder that still holds its
// job file belongs to an update in progress and is left alone.
func removeFinishedRuns(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "job.json")); err == nil {
			continue
		}
		removeTree(dir)
	}
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// Earlier versions kept every download in a token-usage folder under the
// user's Downloads. legacyDownloadsBase is replaced in tests so they never
// touch the real folder.
var legacyDownloadsBase = userDownloadsDirectory

// legacyDownloadNames are the folder names earlier versions used under
// Downloads, before and after the program was renamed.
var legacyDownloadNames = []string{"token-usage", "codex-usage"}

// RemoveLegacyDownloads deletes the downloads earlier versions left under
// Downloads: only the program files in the <name>-v<version>-<n> folders they
// created, then those folders and the <name> folder once empty. It acts only
// when this installation's remembered download lies in one of those folders,
// which proves its updater wrote there, and leaves a folder the user chose
// as the download folder alone. A remembered download whose file no longer
// exists is forgotten.
func (m *Manager) RemoveLegacyDownloads() {
	m.mu.Lock()
	last, chosen := m.status.LastDownload, m.status.CustomDownloadDir
	m.mu.Unlock()
	if base, err := legacyDownloadsBase(); err == nil && filepath.IsAbs(base) && last != "" {
		used := false
		for _, name := range legacyDownloadNames {
			if within(last, filepath.Join(base, name)) {
				used = true
			}
		}
		for _, name := range legacyDownloadNames {
			if dir := filepath.Join(base, name); used && !samePath(dir, chosen) {
				removeLegacyDownloadDir(dir, name)
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status.LastDownload == "" {
		return
	}
	if _, err := os.Stat(m.status.LastDownload); os.IsNotExist(err) {
		m.status.LastDownload = ""
		_ = m.save()
	}
}

// within reports whether path lies inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	if runtime.GOOS == "windows" {
		if strings.EqualFold(filepath.Clean(dir), filepath.Clean(path)) {
			return false
		}
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func removeLegacyDownloadDir(dir, name string) {
	run := regexp.MustCompile(`^` + regexp.QuoteMeta(name) + `-v\d+\.\d+\.\d+-\d+$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !run.MatchString(entry.Name()) {
			continue
		}
		folder := filepath.Join(dir, entry.Name())
		files, err := os.ReadDir(folder)
		if err != nil {
			continue
		}
		for _, file := range files {
			if !file.IsDir() && strings.HasPrefix(file.Name(), name+"-") {
				_ = os.Remove(filepath.Join(folder, file.Name()))
			}
		}
		removeEmptyDir(folder)
	}
	removeEmptyDir(dir)
}

// removeFinishedUpdate deletes the work folders, with their backups, once an
// update has finished: the new version is running ("updated") or the old one
// was restored ("rolled_back"), and the helper that wrote the result has
// exited. The automatic rollback has happened by then, so the backups have no
// further use. A failed update keeps them for manual recovery.
func (m *Manager) removeFinishedUpdate() {
	var result Result
	data, err := os.ReadFile(ResultPath(m.stateDir))
	if err != nil || json.Unmarshal(data, &result) != nil {
		return
	}
	if (result.Phase != "updated" && result.Phase != "rolled_back") || processAlive(result.PID) {
		return
	}
	removeFinishedRuns(filepath.Join(m.stateDir, ".token-usage-updates"))
	removeEmptyDir(filepath.Join(m.stateDir, ".token-usage-updates"))
}

// removeEmptyDir removes a folder when it is empty. Synced folders such as
// OneDrive can mark folders read-only, and Windows refuses to remove those,
// so the flag is cleared when the first attempt fails.
func removeEmptyDir(path string) {
	if err := os.Remove(path); err == nil || os.IsNotExist(err) {
		return
	}
	if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
		return
	}
	if os.Chmod(path, 0o700) == nil {
		_ = os.Remove(path)
	}
}

// removeTree removes a folder and everything in it, clearing read-only
// flags on its folders when the first attempt leaves something behind.
func removeTree(path string) {
	if os.RemoveAll(path) == nil {
		return
	}
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(path)
}
