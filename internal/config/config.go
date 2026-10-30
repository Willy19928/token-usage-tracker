package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"tokenusage/internal/pricing"
)

const (
	DefaultPort        = 43189
	databaseName       = "usage.sqlite"
	legacyManagedBegin = "# BEGIN codex-usage managed"
	legacyManagedEnd   = "# END codex-usage managed"
)

type Config struct {
	ListenAddress       string                      `json:"listen_address"`
	Port                int                         `json:"port"`
	ScanIntervalSeconds int                         `json:"scan_interval_seconds"`
	ExtraCodexHomes     []string                    `json:"extra_codex_homes,omitempty"`
	PricingOverrides    map[string]pricing.Override `json:"pricing_overrides,omitempty"`
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

func ResolvePaths() (Paths, error) {
	if override := strings.TrimSpace(os.Getenv("CODEX_USAGE_HOME")); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return Paths{}, err
		}
		if err := validateDedicatedStateDir(abs); err != nil {
			return Paths{}, err
		}
		name := "codex-usage"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		return Paths{
			StateDir:     abs,
			ConfigPath:   filepath.Join(abs, "config.json"),
			Database:     filepath.Join(abs, databaseName),
			BackupDir:    filepath.Join(abs, "backups"),
			InstallDir:   filepath.Join(abs, "bin"),
			InstalledEXE: filepath.Join(abs, "bin", name),
		}, nil
	}

	if data, ok := PortableDataDir(); ok {
		stateDir := filepath.Join(data, "codex")
		if err := validateDedicatedStateDir(stateDir); err != nil {
			return Paths{}, err
		}
		executable, _ := os.Executable()
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		return Paths{
			StateDir:     stateDir,
			ConfigPath:   filepath.Join(stateDir, "config.json"),
			Database:     filepath.Join(stateDir, databaseName),
			BackupDir:    filepath.Join(stateDir, "backups"),
			InstallDir:   filepath.Dir(executable),
			InstalledEXE: executable,
		}, nil
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
		stateDir = filepath.Join(base, "codex-usage")
		installDir = filepath.Join(base, "Programs", "codex-usage")
	} else if runtime.GOOS == "darwin" {
		stateDir = filepath.Join(home, "Library", "Application Support", "codex-usage")
		installDir = filepath.Join(stateDir, "bin")
	} else {
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		stateDir = filepath.Join(data, "codex-usage")
		installDir = filepath.Join(home, ".local", "bin")
	}
	if err := validateDedicatedStateDir(stateDir); err != nil {
		return Paths{}, err
	}
	name := "codex-usage"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return Paths{
		StateDir:     stateDir,
		ConfigPath:   filepath.Join(stateDir, "config.json"),
		Database:     filepath.Join(stateDir, databaseName),
		BackupDir:    filepath.Join(stateDir, "backups"),
		InstallDir:   installDir,
		InstalledEXE: filepath.Join(installDir, name),
	}, nil
}

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
		return fmt.Errorf("CODEX_USAGE_HOME 不能是檔案系統根目錄")
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && equalPath(clean, home) {
		return fmt.Errorf("CODEX_USAGE_HOME 不能是使用者主目錄")
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
	marker, markerErr := os.ReadFile(filepath.Join(clean, ".codex-usage-state"))
	if markerErr == nil && strings.TrimSpace(string(marker)) == "codex-usage-state-v1" {
		return nil
	}
	managed := map[string]bool{
		"backups": true, "bin": true, "config.json": true, "daemon.log": true,
		databaseName: true, databaseName + "-shm": true, databaseName + "-wal": true,
		databaseName + "-journal": true, "codex-usage.pid": true,
		"codex-usage-start.vbs": true, ".codex-usage-state": true,
	}
	previousName := previousProductName()
	for _, name := range []string{
		previousDatabaseName(), previousDatabaseName() + "-shm", previousDatabaseName() + "-wal",
		previousDatabaseName() + "-journal", previousName + ".pid", previousName + "-start.vbs",
		"." + previousName + "-state",
	} {
		managed[name] = true
	}
	for _, entry := range entries {
		if !managed[entry.Name()] && !strings.HasPrefix(entry.Name(), ".codex-usage-") {
			return fmt.Errorf("%s 不是專用 Token Usage 狀態目錄（發現 %q）；請選擇新的空目錄", clean, entry.Name())
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
	return atomicWrite(filepath.Join(paths.StateDir, ".codex-usage-state"),
		[]byte("codex-usage-state-v1\n"), 0o600)
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

func CodexConfigPath(home string) string {
	return filepath.Join(home, "config.toml")
}

func HasLegacyManagedOTel(home string) (bool, error) {
	data, err := os.ReadFile(CodexConfigPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Contains(data, []byte(legacyManagedBegin)) ||
		bytes.Contains(data, []byte(previousManagedBegin())), nil
}

// RemoveLegacyManagedOTel is upgrade cleanup for releases that used to add a
// managed exporter. It removes only our marker-delimited stanza and leaves any
// user or third-party [otel] configuration untouched.
func RemoveLegacyManagedOTel(home string) (bool, error) {
	path := CodexConfigPath(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	begin, endMarker := legacyManagedBegin, legacyManagedEnd
	start := bytes.Index(data, []byte(begin))
	if start < 0 {
		begin, endMarker = previousManagedBegin(), previousManagedEnd()
		start = bytes.Index(data, []byte(begin))
		if start < 0 {
			return false, nil
		}
	}
	endRel := bytes.Index(data[start:], []byte(endMarker))
	if endRel < 0 {
		return false, fmt.Errorf("發現不完整的 codex-usage managed stanza，拒絕自動修改")
	}
	end := start + endRel + len(endMarker)
	if end < len(data) && data[end] == '\r' {
		end++
	}
	if end < len(data) && data[end] == '\n' {
		end++
	}
	updated := append([]byte{}, data[:start]...)
	updated = append(updated, data[end:]...)
	updated = bytes.TrimRight(updated, "\r\n")
	if len(updated) > 0 {
		updated = append(updated, '\n')
	}
	var parsed map[string]any
	if len(updated) > 0 {
		if err := toml.Unmarshal(updated, &parsed); err != nil {
			return false, fmt.Errorf("移除後 TOML 驗證失敗，未修改: %w", err)
		}
	}
	if err := atomicWriteWithRollback(path, data, updated, 0o600); err != nil {
		return false, err
	}
	return true, nil
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

func randomSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "backup"
	}
	return hex.EncodeToString(buf[:])
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-usage-*")
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
			swap := path + ".codex-usage-swap"
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

func atomicWriteWithRollback(path string, original, updated []byte, mode os.FileMode) error {
	if err := atomicWrite(path, updated, mode); err != nil {
		return fmt.Errorf("原子寫入 %s: %w", path, err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, updated) {
		_ = atomicWrite(path, original, mode)
		if err != nil {
			return fmt.Errorf("寫入後讀取驗證失敗並已復原: %w", err)
		}
		return fmt.Errorf("寫入後內容驗證失敗並已復原")
	}
	return nil
}
