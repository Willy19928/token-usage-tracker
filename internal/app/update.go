package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"tokenusage/internal/config"
	"tokenusage/internal/platform"
	"tokenusage/internal/updater"
)

// The helper runs from a copy inside the state folder, where it cannot tell a
// portable installation apart, so the job names the program to replace.
type updateJob struct {
	Version    string `json:"version"`
	Digest     string `json:"digest"`
	Managed    bool   `json:"managed"`
	Executable string `json:"executable"`
}

func newUpdater(paths config.Paths) *updater.Manager {
	paths = updatePaths(paths)
	executable, _ := os.Executable()
	current, err1 := os.Stat(executable)
	installed, err2 := os.Stat(paths.InstalledEXE)
	var apply func(string, string, string) error
	if err1 == nil && err2 == nil && os.SameFile(current, installed) {
		apply = func(candidate, version, digest string) error {
			dir := filepath.Dir(candidate)
			helper := filepath.Join(dir, "update-helper")
			if runtime.GOOS == "windows" {
				helper += ".exe"
			}
			if err := copyExecutable(executable, helper); err != nil {
				return err
			}
			job := updateJob{Version: version, Digest: digest, Managed: platform.ManagedUpdateService(), Executable: paths.InstalledEXE}
			path := filepath.Join(dir, "job.json")
			if err := updater.WriteJSON(path, job); err != nil {
				return err
			}
			// The helper and the restarted service inherit this, so both resolve
			// the same state folder even when this copy was started without it.
			if err := os.Setenv("TOKEN_USAGE_HOME", paths.StateDir); err != nil {
				return err
			}
			return platform.StartUpdateHelper(helper, path, paths.StateDir, job.Managed)
		}
	}
	m := updater.New(paths.StateDir, Version, runtime.GOOS, runtime.GOARCH, apply)
	if platform.HasGUI() {
		m.SetDirectoryOpener(platform.OpenDirectory)
	}
	return m
}

func (c CLI) applyUpdate(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("需要一個更新工作檔")
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}
	paths = updatePaths(paths)
	path, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	root := filepath.Join(paths.StateDir, ".token-usage-updates")
	helper, _ := os.Executable()
	if filepath.Base(path) != "job.json" || filepath.Dir(dir) != root || !strings.HasPrefix(filepath.Base(dir), "run-") || filepath.Dir(helper) != dir {
		return fmt.Errorf("更新工作檔的位置無效")
	}
	// Reject symlinked directories before touching the installed binary or data.
	for _, p := range []string{root, dir, path} {
		info, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("更新工作檔不能是符號連結")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var job updateJob
	if err = json.Unmarshal(data, &job); err != nil {
		return err
	}
	if !updater.Newer(job.Version, Version) {
		return fmt.Errorf("只能更新到較新的穩定版")
	}
	if job.Executable != "" {
		if !filepath.IsAbs(job.Executable) || filepath.Base(job.Executable) != filepath.Base(paths.InstalledEXE) {
			return fmt.Errorf("更新目標不是本程式")
		}
		if _, err := os.Stat(job.Executable); err != nil {
			return err
		}
		paths.InstalledEXE = filepath.Clean(job.Executable)
		paths.InstallDir = filepath.Dir(paths.InstalledEXE)
	}
	defer func() { _ = os.Remove(path) }() // A completed job cannot be replayed.
	if err = updater.WriteResult(paths.StateDir, updater.Result{Phase: "installing", Target: job.Version}); err != nil {
		return err
	}
	phase, runErr := runUpdate(paths, dir, job)
	result := updater.Result{Phase: phase, Target: job.Version}
	if runErr != nil {
		result.Error = runErr.Error()
	}
	if err = updater.WriteResult(paths.StateDir, result); err != nil {
		return err
	}
	return runErr
}

// updatePaths points the update at the program that actually runs. A portable
// copy is started with TOKEN_USAGE_HOME set to its data folder, which would
// otherwise resolve to a bin folder inside it; the program itself is the
// installation there. Other installs may record their location at install time.
func updatePaths(paths config.Paths) config.Paths {
	if data, ok := config.PortableDataDir(); ok && samePath(data, paths.StateDir) {
		if executable, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(executable); err == nil {
				executable = resolved
			}
			paths.InstalledEXE = executable
			paths.InstallDir = filepath.Dir(executable)
			return paths
		}
	}
	var record struct {
		Executable string `json:"executable"`
	}
	data, err := os.ReadFile(installRecordPath(paths.StateDir))
	if err == nil && json.Unmarshal(data, &record) == nil && filepath.IsAbs(record.Executable) && filepath.Base(record.Executable) == filepath.Base(paths.InstalledEXE) {
		paths.InstalledEXE = filepath.Clean(record.Executable)
		paths.InstallDir = filepath.Dir(paths.InstalledEXE)
		return paths
	}
	// The service is started with TOKEN_USAGE_HOME, which puts the program in a
	// bin folder inside the service folder; a standard Windows or Linux install
	// keeps it elsewhere. Installs made before the install record existed are
	// found through the launcher that starts the service, and remembered.
	if _, statErr := os.Stat(paths.InstalledEXE); statErr != nil {
		if executable, ok := serviceExecutable(paths.StateDir); ok && filepath.IsAbs(executable) &&
			filepath.Base(executable) == filepath.Base(paths.InstalledEXE) {
			if _, statErr := os.Stat(executable); statErr == nil {
				paths.InstalledEXE = filepath.Clean(executable)
				paths.InstallDir = filepath.Dir(paths.InstalledEXE)
				_ = writeInstallRecord(paths.StateDir, paths.InstalledEXE)
			}
		}
	}
	return paths
}

// serviceExecutable is replaceable in tests.
var serviceExecutable = platform.ServiceExecutable

func installRecordPath(stateDir string) string {
	return filepath.Join(stateDir, ".token-usage-install.json")
}

// writeInstallRecord remembers where the installed program is, so a service
// started with TOKEN_USAGE_HOME can still update it in place.
func writeInstallRecord(stateDir, executable string) error {
	return updater.WriteJSON(installRecordPath(stateDir), map[string]string{"executable": executable})
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// updateDataFiles lists every database the new version may migrate, so a
// failed update can restore all of them from the stopped-service snapshot.
func updateDataFiles(paths config.Paths) []string {
	files := []string{paths.ConfigPath}
	claudeDir, agDir := ledgerDirs(paths)
	for _, db := range []string{paths.Database, filepath.Join(claudeDir, "usage.sqlite"), filepath.Join(agDir, "usage.sqlite"), filepath.Join(limitsDir(paths), "limits.sqlite")} {
		files = append(files, db, db+"-wal", db+"-shm", db+"-journal")
	}
	return files
}

func runUpdate(paths config.Paths, dir string, job updateJob) (string, error) {
	candidate := filepath.Join(dir, "candidate")
	if runtime.GOOS == "windows" {
		candidate += ".exe"
	}
	if err := updater.Verify(candidate, job.Digest); err != nil {
		return "failed", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, candidate, "--version").Output()
	if err != nil || !strings.HasPrefix(string(output), "token-usage "+job.Version+" ") {
		return "failed", fmt.Errorf("下載的新版無法執行，或版本與預期不符")
	}
	cfg, err := config.Load(paths)
	if err != nil {
		return "failed", err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port)
	t := updater.Transaction{
		Directory: dir, Candidate: candidate, Executable: paths.InstalledEXE, Digest: job.Digest,
		DataFiles: updateDataFiles(paths),
		Copy:      copyUpdateFile,
		Stop:      func() error { return platform.StopForUpdate(paths.InstalledEXE, paths.StateDir, job.Managed) },
		Start:     func() error { return platform.StartAfterUpdate(paths.InstalledEXE, job.Managed) },
		Healthy:   func() error { return waitUpdateHealth(url, job.Version) },
	}
	phase, err := t.Run()
	if phase == "rolled_back" {
		if healthErr := waitUpdateHealth(url, Version); healthErr != nil {
			return "failed", fmt.Errorf("%v；舊版沒有恢復運作: %w", err, healthErr)
		}
	}
	return phase, err
}

func copyUpdateFile(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if err = copyExecutable(source, destination); err != nil {
		return err
	}
	return os.Chmod(destination, info.Mode().Perm())
}

func waitUpdateHealth(url, version string) error {
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(url)
		if err == nil {
			var body struct {
				OK      bool   `json:"ok"`
				Version string `json:"version"`
			}
			err = json.NewDecoder(res.Body).Decode(&body)
			res.Body.Close()
			if err == nil && res.StatusCode == 200 && body.OK && body.Version == version {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("%s 版在 45 秒內沒有正常啟動", version)
}
