package updater

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadDirectoryPersistsValidatesAndOpens(t *testing.T) {
	m, _ := testManager(t, false, nil)
	want := filepath.Join(t.TempDir(), "updates with spaces", "下载")
	off := false
	if err := m.SetPreferences(&off, &want); err != nil {
		t.Fatal(err)
	}
	n := New(m.stateDir, "2.4.1", "windows", "amd64", nil)
	if s := n.Status(); s.DownloadDir != want || s.CustomDownloadDir != want || s.AutoCheck {
		t.Fatal(s)
	}
	opened := ""
	n.SetDirectoryOpener(func(path string) error { opened = path; return nil })
	if err := n.OpenDownloadDirectory(); err != nil || opened != want {
		t.Fatal(opened, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"relative/path", file, want + "\nwrong"} {
		on := true
		if err := n.SetPreferences(&on, &bad); err == nil {
			t.Fatal("accepted invalid directory", bad)
		}
		if s := n.Status(); s.DownloadDir != want || s.AutoCheck {
			t.Fatal("partial preferences saved", s)
		}
	}
	if err := WriteResult(n.stateDir, Result{Phase: "installing", Target: "2.5.0"}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if err := n.SetPreferences(nil, &other); err == nil {
		t.Fatal("changed directory during installation")
	}
	if err := WriteResult(n.stateDir, Result{Phase: "failed", Target: "2.5.0"}); err != nil {
		t.Fatal(err)
	}
	// Use an isolated default for this test; never create directories in the real Downloads.
	n.status.DefaultDownloadDir = filepath.Join(t.TempDir(), "default")
	empty := ""
	if err := n.SetPreferences(nil, &empty); err != nil {
		t.Fatal(err)
	}
	if s := n.Status(); s.DownloadDir != s.DefaultDownloadDir || s.CustomDownloadDir != "" {
		t.Fatal(s)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal("old download directory removed", err)
	}
}

func TestDownloadUsesSelectedDirectoryAndPrivateStaging(t *testing.T) {
	applied := make(chan string, 1)
	m, _ := testManager(t, false, func(path, version, digest string) error {
		if err := Verify(path, digest); err != nil {
			return err
		}
		applied <- path
		return nil
	})
	if err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Install("2.5.0"); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return len(applied) > 0 })
	private := <-applied
	if !strings.HasPrefix(private, filepath.Join(m.stateDir, ".token-usage-updates")+string(os.PathSeparator)) {
		t.Fatal("helper staging escaped state directory", private)
	}
	s := m.Status()
	if filepath.Dir(filepath.Dir(s.LastDownload)) != s.DownloadDir || filepath.Base(s.LastDownload) != m.asset || !strings.HasPrefix(filepath.Base(filepath.Dir(s.LastDownload)), "token-usage-v2.5.0-") {
		t.Fatal(s)
	}
	entries, err := os.ReadDir(filepath.Dir(s.LastDownload))
	if err != nil || len(entries) != 1 {
		t.Fatal("backup/helper leaked into download folder", entries, err)
	}
	if New(m.stateDir, "2.5.0", "windows", "amd64", nil).Status().LastDownload != s.LastDownload {
		t.Fatal("last download path not persisted")
	}
	if err := os.WriteFile(s.LastDownload, []byte("tampered public copy"), 0600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(private)
	if err != nil || string(content) != "test binary contents" {
		t.Fatal("public changes affected staged installer", err)
	}
	if err := stageDownload(s.LastDownload, filepath.Join(t.TempDir(), "staged"), strings.Repeat("0", 64)); err == nil {
		t.Fatal("modified download passed staging check")
	}
}

// Downloads in the default temporary folder are removed once staged, and the
// working folders of earlier updates do not pile up.
func TestDefaultDownloadIsRemovedAndOldRunsArePruned(t *testing.T) {
	applied := make(chan string, 1)
	m, _ := testManager(t, false, func(path, version, digest string) error {
		applied <- path
		return nil
	})
	temp := filepath.Join(t.TempDir(), "token-usage-updates")
	m.mu.Lock()
	m.status.DefaultDownloadDir, m.status.DownloadDir, m.status.CustomDownloadDir = temp, temp, ""
	m.mu.Unlock()
	root := filepath.Join(m.stateDir, ".token-usage-updates")
	finished, running := filepath.Join(root, "run-old"), filepath.Join(root, "run-busy")
	for _, dir := range []string{finished, running} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(running, "job.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Install("2.5.0"); err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { return len(applied) > 0 })
	staged := <-applied
	if _, err := os.Stat(staged); err != nil {
		t.Fatal("staged installer missing", err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("temporary download folder left behind", err)
	}
	if m.Status().LastDownload != "" {
		t.Fatal("last download points to a removed file", m.Status().LastDownload)
	}
	if _, err := os.Stat(finished); !os.IsNotExist(err) {
		t.Fatal("finished update folder kept", err)
	}
	if _, err := os.Stat(running); err != nil {
		t.Fatal("update in progress removed", err)
	}
}

// Downloads that earlier versions kept under Downloads are removed when this
// installation's updater wrote there; anything else, other installations'
// folders and a folder the user chose stay.
func TestLegacyDownloadsAreRemoved(t *testing.T) {
	base := t.TempDir()
	saved := legacyDownloadsBase
	legacyDownloadsBase = func() (string, error) { return base, nil }
	defer func() { legacyDownloadsBase = saved }()
	dir := filepath.Join(base, "token-usage")
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	remember := func(m *Manager, path string) {
		m.mu.Lock()
		m.status.LastDownload = path
		m.mu.Unlock()
	}
	old := filepath.Join(dir, "token-usage-v1.1.2-123456", "token-usage-windows-amd64.exe")
	older := filepath.Join(dir, "token-usage-v1.1.1-987", "token-usage-linux-amd64")
	renamed := filepath.Join(base, "codex-usage", "codex-usage-v1.0.4-42", "codex-usage-windows-amd64.exe")
	write(old)
	write(older)
	write(renamed)
	m, _ := testManager(t, false, nil)

	// No remembered download there: another installation's files stay.
	m.RemoveLegacyDownloads()
	if _, err := os.Stat(old); err != nil {
		t.Fatal("cleaned without proof of this installation's downloads", err)
	}

	// Its updater wrote there: both the current and the pre-rename folders go,
	// and the remembered download, now gone, is forgotten.
	remember(m, old)
	m.RemoveLegacyDownloads()
	for _, gone := range []string{dir, filepath.Join(base, "codex-usage")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatal("legacy download folder kept", gone, err)
		}
	}
	if m.Status().LastDownload != "" {
		t.Fatal("missing last download still shown", m.Status().LastDownload)
	}

	// Unrelated files stay.
	write(old)
	mine := filepath.Join(dir, "notes.txt")
	write(mine)
	remember(m, old)
	m.RemoveLegacyDownloads()
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Fatal("legacy download kept", err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("unrelated file removed", err)
	}

	// A folder the user chose is left alone.
	write(old)
	if err := m.SetPreferences(nil, &dir); err != nil {
		t.Fatal(err)
	}
	remember(m, old)
	m.RemoveLegacyDownloads()
	if _, err := os.Stat(old); err != nil {
		t.Fatal("chosen download folder was cleaned", err)
	}
}

// Work folders and backups go once an update has finished and its helper has
// exited; a failed update keeps them, and so does one still in progress.
func TestFinishedUpdateRemovesWorkFolders(t *testing.T) {
	m, _ := testManager(t, false, nil)
	root := filepath.Join(m.stateDir, ".token-usage-updates")
	setup := func() (done, running string) {
		t.Helper()
		done, running = filepath.Join(root, "run-done"), filepath.Join(root, "run-busy")
		for _, dir := range []string{done, running} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "backup-0"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(running, "job.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return done, running
	}
	result := func(phase string, pid int) {
		t.Helper()
		if err := WriteJSON(ResultPath(m.stateDir), Result{PID: pid, Phase: phase, Target: "2.5.0"}); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(path string) bool { _, err := os.Stat(path); return err == nil }

	done, running := setup()
	result("failed", 0)
	m.removeFinishedUpdate()
	if !exists(done) {
		t.Fatal("failed update lost its backups")
	}
	result("updated", os.Getpid()) // the helper is still running
	m.removeFinishedUpdate()
	if !exists(done) {
		t.Fatal("removed while the helper was running")
	}
	for _, phase := range []string{"updated", "rolled_back"} {
		done, running = setup()
		result(phase, 0)
		m.removeFinishedUpdate()
		if exists(done) || !exists(running) {
			t.Fatalf("%s: done kept=%v, in progress kept=%v", phase, exists(done), exists(running))
		}
	}
}

// Synced folders can be read-only; emptied folders and finished work folders
// are still removed.
func TestRemovalClearsReadOnlyFolders(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	tree := filepath.Join(root, "tree")
	inner := filepath.Join(tree, "inner")
	for _, dir := range []string{empty, inner} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inner, "backup-0"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{empty, inner, tree} {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range []string{inner, tree, empty} {
			_ = os.Chmod(dir, 0o700)
		}
	})
	removeEmptyDir(empty)
	removeTree(tree)
	for _, dir := range []string{empty, tree} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("read-only folder kept", dir, err)
		}
	}
}
