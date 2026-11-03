package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"tokenusage/internal/config"
	"tokenusage/internal/platform"
	"tokenusage/internal/updater"
)

// Exercise real executable replacement of a portable copy, started the way the
// login launcher starts it, without touching startup entries, the running
// service or any real Codex, Claude Code or Antigravity data.
func TestUpdatePortableServicePreservesAllLedgers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two application versions")
	}
	root := t.TempDir()
	appDir := filepath.Join(root, "Token Usage")
	stateDir := filepath.Join(appDir, "data", "codex")
	home := filepath.Join(root, "codex")
	claudeHome := filepath.Join(root, "claude")
	t.Setenv("CODEX_USAGE_HOME", stateDir)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("ANTIGRAVITY_TRACKER_HOME", filepath.Join(root, "gemini"))
	t.Setenv("CLAUDE_TRACKER_DATA", "")
	t.Setenv("ANTIGRAVITY_TRACKER_DATA", "")
	for _, dir := range []string{filepath.Join(home, "sessions"), filepath.Join(claudeHome, "projects", "fixture"), filepath.Join(appDir, "data", "claude"), filepath.Join(appDir, "data", "antigravity")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := config.Default()
	cfg.Port = port
	if err = config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
	if err = config.EnsureStateMarker(paths); err != nil {
		t.Fatal(err)
	}
	if err = updater.WriteJSON(filepath.Join(stateDir, ".codex-usage-updates.json"), map[string]bool{"auto_check": false}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format(time.RFC3339)
	codexFixture := filepath.Join(home, "sessions", "rollout-update.jsonl")
	fixture := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"update-test","cwd":"/fixture"}}
{"timestamp":%q,"type":"turn_context","payload":{"turn_id":"turn-update","model":"gpt-5.4"}}
{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":80,"output_tokens":20,"total_tokens":100},"last_token_usage":{"input_tokens":80,"output_tokens":20,"total_tokens":100}}}}
`, at, at, at)
	if err = os.WriteFile(codexFixture, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeFixture := filepath.Join(claudeHome, "projects", "fixture", "session.jsonl")
	claudeLine := fmt.Sprintf(`{"type":"assistant","sessionId":"s1","uuid":"u1","requestId":"req1","timestamp":%q,"message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":30,"output_tokens":20}}}`+"\n", at)
	if err = os.WriteFile(claudeFixture, []byte(claudeLine), 0o600); err != nil {
		t.Fatal(err)
	}

	installed := filepath.Join(appDir, "codex-usage")
	dir := filepath.Join(stateDir, ".codex-usage-updates", "run-integration")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate")
	helper := filepath.Join(dir, "update-helper")
	if runtime.GOOS == "windows" {
		installed += ".exe"
		candidate += ".exe"
		helper += ".exe"
	}
	build := func(out, version string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-X tokenusage/internal/app.Version="+version, "-o", out, "../../cmd/codex-usage")
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v %s", err, data)
		}
	}
	build(installed, "2.4.1")
	build(candidate, "2.5.0")
	if err = copyExecutable(installed, helper); err != nil {
		t.Fatal(err)
	}
	old := exec.Command(installed, "serve")
	if err = old.Start(); err != nil {
		t.Fatal(err)
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- old.Wait() }()
	t.Cleanup(func() {
		_ = platform.StopForUpdate(installed, stateDir, false)
		select {
		case <-oldDone:
		case <-time.After(5 * time.Second):
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err = waitUpdateHealth(base+"/healthz", "2.4.1"); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	total := func(provider string) int64 {
		res, err := client.Get(base + "/api/v1/summary?since=all&provider=" + provider)
		if err != nil {
			return -1
		}
		defer res.Body.Close()
		var body struct {
			Total int64 `json:"grand_total"`
		}
		if json.NewDecoder(res.Body).Decode(&body) != nil {
			return -1
		}
		return body.Total
	}
	deadline := time.Now().Add(20 * time.Second)
	for total("codex") != 100 || total("claude") != 50 {
		if time.Now().After(deadline) {
			t.Fatalf("fixtures not scanned: codex=%d claude=%d", total("codex"), total("claude"))
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The running service must report this portable copy as updatable.
	res, err := client.Get(base + "/api/v1/updates")
	if err != nil {
		t.Fatal(err)
	}
	var before updater.Status
	err = json.NewDecoder(res.Body).Decode(&before)
	res.Body.Close()
	if err != nil || !before.CanInstall {
		t.Fatalf("portable copy cannot install updates: %+v %v", before, err)
	}
	// Removing the sources is intentional: an update must keep the existing
	// ledgers rather than rebuild them from the files still present.
	for _, path := range []string{codexFixture, claudeFixture} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	jobPath := filepath.Join(dir, "job.json")
	if err = updater.WriteJSON(jobPath, updateJob{Version: "2.5.0", Digest: hex.EncodeToString(hash[:]), Executable: installed}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, helper, "_apply-update", jobPath).CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, output)
	}
	if err = waitUpdateHealth(base+"/healthz", "2.5.0"); err != nil {
		t.Fatal(err)
	}
	if codex, claude := total("codex"), total("claude"); codex != 100 || claude != 50 {
		t.Fatalf("statistics changed across update: codex=%d claude=%d", codex, claude)
	}
	var result updater.Result
	data, _ = os.ReadFile(updater.ResultPath(stateDir))
	if err = json.Unmarshal(data, &result); err != nil || result.Phase != "updated" {
		t.Fatalf("%s %v", data, err)
	}
	if output, err := exec.Command(installed, "--version").Output(); err != nil || string(output[:len("codex-usage 2.5.0 ")]) != "codex-usage 2.5.0 " {
		t.Fatalf("installed program was not replaced: %s %v", output, err)
	}
	for _, name := range []string{"previous-program", "backup-0", "backup-5"} {
		if _, err = os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing backup %s: %v", name, err)
		}
	}
}
