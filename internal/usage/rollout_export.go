package usage

import (
	"io"
	"os"
)

// WalkRollouts lists the logical rollout paths of a Codex home, mapping a
// compressed copy to its .jsonl name the same way the accounting scanner does.
func WalkRollouts(home string) []string { return walkRollouts(home) }

// StatRollout stats a logical rollout path; a compressed copy reports its
// decompressed size.
func StatRollout(path string) (os.FileInfo, error) { return statRollout(path) }

// OpenRolloutAt opens a logical rollout path positioned at offset in its
// decompressed content.
func OpenRolloutAt(path string, offset int64) (io.ReadCloser, error) {
	return openRolloutAt(path, offset)
}
