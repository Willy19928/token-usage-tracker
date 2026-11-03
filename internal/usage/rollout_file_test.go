package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"tokenusage/internal/store"
)

func writeZstd(t *testing.T, path string, frames ...string) {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	var data []byte
	for _, frame := range frames {
		data = encoder.EncodeAll([]byte(frame), data)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCompressedRolloutContinuesFromSavedOffset(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	plain := filepath.Join(home, "sessions", "2026", "rollout-a.jsonl")
	writeDiscoveryState(t, home, "a", plain)
	if err := os.MkdirAll(filepath.Dir(plain), 0o700); err != nil {
		t.Fatal(err)
	}
	scanned := `{"type":"session_meta","payload":{"id":"a"}}` + "\n" +
		tokenLine("2026-10-19T01:00:00Z", usage(8, 1, 0, 2, 0, 10), usage(8, 1, 0, 2, 0, 10)) + "\n" +
		tokenLine("2026-10-19T01:01:00Z", usage(20, 5, 0, 5, 0, 25), usage(12, 4, 0, 3, 0, 15)) + "\n"
	tail := tokenLine("2026-10-19T01:02:00Z", usage(30, 9, 0, 10, 0, 40), usage(10, 4, 0, 5, 0, 15)) + "\n"
	if err := os.WriteFile(plain, []byte(scanned), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	scanner := &Scanner{Store: st}
	if first, err := scanner.Scan(ctx, []string{home}, false); err != nil || first.EventsInserted != 2 {
		t.Fatalf("first scan = %+v, %v", first, err)
	}

	// Codex appends the last turn while the tracker is stopped, then replaces
	// the rollout with a compressed copy. Two frames exercise the size walk.
	writeZstd(t, plain+".zst", scanned, tail)
	if err := os.Remove(plain); err != nil {
		t.Fatal(err)
	}
	info, err := statRollout(plain)
	if err != nil || info.Size() != int64(len(scanned+tail)) || !isCompressedRollout(info) {
		t.Fatalf("compressed stat = %v, %v", info, err)
	}
	second, err := scanner.Scan(ctx, []string{home}, false)
	if err != nil || second.EventsInserted != 1 || second.Duplicates != 0 {
		t.Fatalf("compressed scan = %+v, %v", second, err)
	}
	if third, err := scanner.Scan(ctx, []string{home}, false); err != nil || third.EventsInserted != 0 || third.Records != 0 {
		t.Fatalf("unchanged compressed rollout was read again: %+v, %v", third, err)
	}
	warnings, err := st.Warnings(ctx, 10)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings = %+v, %v", warnings, err)
	}
}

func TestPlainRolloutWinsOverCompressedCopy(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "rollout-b.jsonl")
	if err := os.WriteFile(plain, []byte("plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeZstd(t, plain+".zst", "compressed copy\n")
	if _, ok := logicalRolloutPath(plain + ".zst"); ok {
		t.Fatal("compressed copy listed while the plain rollout exists")
	}
	if info, err := statRollout(plain); err != nil || isCompressedRollout(info) {
		t.Fatalf("stat = %v, %v", info, err)
	}
}
