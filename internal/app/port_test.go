package app

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"

	"tokenusage/internal/cliui"
	"tokenusage/internal/config"
)

func TestEnsurePortMovesAwayFromAnotherProgram(t *testing.T) {
	// A plain TCP listener stands in for another program: it holds the port
	// but does not answer the health check.
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	taken := other.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	paths := config.Paths{StateDir: dir, ConfigPath: filepath.Join(dir, "config.json")}
	cfg := config.Default()
	cfg.Port = taken
	var out bytes.Buffer
	cli := CLI{Stdout: &out, Stderr: &out, locale: cliui.English}
	if err := cli.ensurePort(paths, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Port == taken || !portFree(cfg.ListenAddress, cfg.Port) {
		t.Fatalf("port = %d, taken = %d", cfg.Port, taken)
	}
	saved, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Port != cfg.Port {
		t.Fatalf("saved port = %d, want %d", saved.Port, cfg.Port)
	}
	if !bytes.Contains(out.Bytes(), []byte("switched to")) {
		t.Fatalf("no notice printed: %q", out.String())
	}
}

func TestEnsurePortKeepsAFreePort(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	dir := t.TempDir()
	paths := config.Paths{StateDir: dir, ConfigPath: filepath.Join(dir, "config.json")}
	cfg := config.Default()
	cfg.Port = port
	cli := CLI{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, locale: cliui.English}
	if err := cli.ensurePort(paths, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != port {
		t.Fatalf("port changed to %d", cfg.Port)
	}
}

func TestLogTailReturnsOnlyNewLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writeFile(t, path, "old line\n")
	start := fileSize(path)
	writeFile(t, path, "old line\none\ntwo\nthree\n")
	if got := logTail(path, start, 2); got != "two\nthree" {
		t.Fatalf("logTail = %q", got)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
