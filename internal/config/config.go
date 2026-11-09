package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"tokenusage/internal/pricing"
)

const (
	DefaultPort  = 43189
	databaseName = "usage.sqlite"
)

type Config struct {
	ListenAddress       string                      `json:"listen_address"`
	Port                int                         `json:"port"`
	ScanIntervalSeconds int                         `json:"scan_interval_seconds"`
	ExtraCodexHomes     []string                    `json:"extra_codex_homes,omitempty"`
	PricingOverrides    map[string]pricing.Override `json:"pricing_overrides,omitempty"`
	// ClaudeUsageAPI lets the service read Claude Code's saved login to poll
	// the official usage endpoint, refreshing that login when it expires.
	ClaudeUsageAPI bool `json:"claude_usage_api,omitempty"`
}

type Paths struct {
	StateDir     string
	ConfigPath   string
	Database     string
	BackupDir    string
	InstallDir   string
	InstalledEXE string
}

func Default() Config {
	return Config{
		ListenAddress:       "127.0.0.1",
		Port:                DefaultPort,
		ScanIntervalSeconds: 600,
	}
}

// ResolvePaths finds the service folder: TOKEN_USAGE_HOME, the portable data
// folder beside the program, or the per-user application folder. The service
// keeps its own files at the top of it and each ledger in a subfolder.
func ResolvePaths() (Paths, error) {
	name := "token-usage"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if override := strings.TrimSpace(os.Getenv("TOKEN_USAGE_HOME")); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return Paths{}, err
		}
		if err := validateDedicatedStateDir(abs); err != nil {
			return Paths{}, err
		}
		return servicePaths(abs, filepath.Join(abs, "bin"), filepath.Join(abs, "bin", name)), nil
	}

	if data, ok := PortableDataDir(); ok {
		if err := validateDedicatedStateDir(data); err != nil {
			return Paths{}, err
		}
		executable, _ := os.Executable()
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		return servicePaths(data, filepath.Dir(executable), executable), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	var stateDir, installDir string
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		stateDir = filepath.Join(base, "token-usage")
		installDir = filepath.Join(base, "Programs", "token-usage")
	} else if runtime.GOOS == "darwin" {
		stateDir = filepath.Join(home, "Library", "Application Support", "token-usage")
		installDir = filepath.Join(stateDir, "bin")
	} else {
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		stateDir = filepath.Join(data, "token-usage")
		installDir = filepath.Join(home, ".local", "bin")
	}
	if err := validateDedicatedStateDir(stateDir); err != nil {
		return Paths{}, err
	}
	return servicePaths(stateDir, installDir, filepath.Join(installDir, name)), nil
}

func servicePaths(stateDir, installDir, executable string) Paths {
	return Paths{
		StateDir:     stateDir,
		ConfigPath:   filepath.Join(stateDir, "config.json"),
		Database:     filepath.Join(stateDir, "codex", databaseName),
		BackupDir:    filepath.Join(stateDir, "backups"),
		InstallDir:   installDir,
		InstalledEXE: executable,
	}
}

// LedgerDir is the folder of one ledger inside the service folder: "codex",
// "claude", "antigravity" or "limits".
func (p Paths) LedgerDir(name string) string { return filepath.Join(p.StateDir, name) }

// PortableDataDir returns the "data" folder beside the running executable
// when it exists. A portable copy keeps every ledger there (one subfolder per
// source), so the program and its data can live together in a synced folder.
func PortableDataDir() (string, bool) {
	executable, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	dir := filepath.Join(filepath.Dir(executable), "data")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, true
	}
	return "", false
}

func validateDedicatedStateDir(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	clean := filepath.Clean(absolute)
	root := filepath.Clean(filepath.VolumeName(clean) + string(os.PathSeparator))
	equalPath := func(a, b string) bool {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
		}
		return filepath.Clean(a) == filepath.Clean(b)
	}
	if equalPath(clean, root) {
		return fmt.Errorf("TOKEN_USAGE_HOME 不能是檔案系統根目錄")
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && equalPath(clean, home) {
		return fmt.Errorf("TOKEN_USAGE_HOME 不能是使用者主目錄")
	}
	entries, readErr := os.ReadDir(clean)
	if errors.Is(readErr, os.ErrNotExist) {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	if len(entries) == 0 {
		return nil
	}
	marker, markerErr := os.ReadFile(filepath.Join(clean, ".token-usage-state"))
	if markerErr == nil && strings.TrimSpace(string(marker)) == "token-usage-state-v1" {
		return nil
	}
	managed := map[string]bool{
		"backups": true, "bin": true, "config.json": true, "daemon.log": true,
		"codex": true, "claude": true, "antigravity": true, "limits": true,
		"token-usage.pid": true, "token-usage-start.vbs": true, ".token-usage-state": true,
	}
	for _, entry := range entries {
		if !managed[entry.Name()] && !strings.HasPrefix(entry.Name(), ".token-usage-") {
			return fmt.Errorf("%s 不是 Token Usage 專用的服務資料夾（發現 %q）；請選擇新的空資料夾", clean, entry.Name())
		}
	}
	return nil
}

func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func Load(paths Paths) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(paths.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析 %s: %w", paths.ConfigPath, err)
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1"
	}
	if cfg.ListenAddress != "127.0.0.1" && cfg.ListenAddress != "localhost" {
		return Config{}, fmt.Errorf("拒絕非 loopback 監聽位址 %q", cfg.ListenAddress)
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("無效連接埠 %d", cfg.Port)
	}
	if cfg.ScanIntervalSeconds < 600 {
		cfg.ScanIntervalSeconds = 600
	}
	cfg.ExtraCodexHomes = cleanHomes(cfg.ExtraCodexHomes)
	cfg.PricingOverrides, err = pricing.NormalizeOverrides(cfg.PricingOverrides)
	if err != nil {
		return Config{}, fmt.Errorf("無效 pricing_overrides: %w", err)
	}
	return cfg, nil
}

func Save(paths Paths, cfg Config) error {
	if err := EnsurePrivateDir(paths.StateDir); err != nil {
		return err
	}
	cfg.ExtraCodexHomes = cleanHomes(cfg.ExtraCodexHomes)
	if cfg.ScanIntervalSeconds < 600 {
		cfg.ScanIntervalSeconds = 600
	}
	var err error
	cfg.PricingOverrides, err = pricing.NormalizeOverrides(cfg.PricingOverrides)
	if err != nil {
		return fmt.Errorf("無效 pricing_overrides: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := atomicWrite(paths.ConfigPath, data, 0o600); err != nil {
		return err
	}
	return EnsureStateMarker(paths)
}

func EnsureStateMarker(paths Paths) error {
	if err := EnsurePrivateDir(paths.StateDir); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(paths.StateDir, ".token-usage-state"),
		[]byte("token-usage-state-v1\n"), 0o600)
}

func AddHome(paths Paths, raw string) (string, error) {
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s 不是目錄", abs)
	}
	cfg, err := Load(paths)
	if err != nil {
		return "", err
	}
	cfg.ExtraCodexHomes = append(cfg.ExtraCodexHomes, abs)
	if err := Save(paths, cfg); err != nil {
		return "", err
	}
	return abs, nil
}

func CodexHomes(cfg Config) ([]string, error) {
	var homes []string
	if explicit := strings.TrimSpace(os.Getenv("CODEX_HOME")); explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return nil, err
		}
		homes = append(homes, abs)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		homes = append(homes, filepath.Join(home, ".codex"))
	}
	homes = append(homes, cfg.ExtraCodexHomes...)
	return cleanHomes(homes), nil
}

func cleanHomes(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		abs, err := filepath.Abs(value)
		if err != nil {
			continue
		}
		key := filepath.Clean(abs)
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, filepath.Clean(abs))
	}
	sort.Strings(out)
	return out
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-usage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(path); err == nil {
			swap := path + ".token-usage-swap"
			_ = os.Remove(swap)
			if err := os.Rename(path, swap); err != nil {
				return err
			}
			if err := os.Rename(tmpName, path); err != nil {
				_ = os.Rename(swap, path)
				return err
			}
			_ = os.Remove(swap)
			return nil
		}
	}
	return os.Rename(tmpName, path)
}
