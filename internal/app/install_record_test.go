package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"tokenusage/internal/config"
)

// A standard install keeps the program outside the service folder, but the
// service runs with TOKEN_USAGE_HOME, which points the program at a bin
// folder inside it. The launcher that starts the service names the real
// program; once found it is remembered in the install record.
func TestUpdatePathsFindsTheInstallThroughTheServiceLauncher(t *testing.T) {
	name := "token-usage"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	installed := filepath.Join(root, "Programs", "token-usage", name)
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("program"), 0o700); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{StateDir: state, InstallDir: filepath.Join(state, "bin"), InstalledEXE: filepath.Join(state, "bin", name)}

	launcher := installed
	original := serviceExecutable
	serviceExecutable = func(string) (string, bool) { return launcher, launcher != "" }
	defer func() { serviceExecutable = original }()

	got := updatePaths(paths)
	if got.InstalledEXE != installed || got.InstallDir != filepath.Dir(installed) {
		t.Fatalf("installed = %q", got.InstalledEXE)
	}
	if _, err := os.Stat(installRecordPath(state)); err != nil {
		t.Fatalf("install record not written: %v", err)
	}
	// Later starts use the record even without the launcher.
	launcher = ""
	if got := updatePaths(paths); got.InstalledEXE != installed {
		t.Fatalf("from record = %q", got.InstalledEXE)
	}
}

// A launcher naming a missing or differently named program is ignored.
func TestUpdatePathsIgnoresALauncherForAnotherProgram(t *testing.T) {
	state := t.TempDir()
	paths := config.Paths{StateDir: state, InstalledEXE: filepath.Join(state, "bin", "token-usage.exe")}
	original := serviceExecutable
	serviceExecutable = func(string) (string, bool) { return filepath.Join(state, "missing", "token-usage.exe"), true }
	defer func() { serviceExecutable = original }()
	if got := updatePaths(paths); got.InstalledEXE != paths.InstalledEXE {
		t.Fatalf("followed a launcher to a missing program: %q", got.InstalledEXE)
	}
	if _, err := os.Stat(installRecordPath(state)); err == nil {
		t.Fatal("wrote an install record for a missing program")
	}
}
