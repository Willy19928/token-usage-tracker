package usage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"tokenusage/internal/model"
	"tokenusage/internal/store"
)

// Codex "undo" and "restore checkpoint" write a new rollout file that keeps
// the session id. Its first token_count is either the restored state's own
// snapshot again (no new usage) or a new request sent from that state (new
// usage on a branch). These tests model one session that is itself a fork.

type vec struct{ in, cached, out, reasoning, total int64 }

// state is a consistent cumulative vector for a given total.
func state(total int64) vec {
	out := total / 10
	in := total - out
	return vec{in: in, cached: in / 2, out: out, reasoning: out / 2, total: total}
}

func (v vec) sub(o vec) vec {
	return vec{v.in - o.in, v.cached - o.cached, v.out - o.out, v.reasoning - o.reasoning, v.total - o.total}
}

func (v vec) add(o vec) vec {
	return vec{v.in + o.in, v.cached + o.cached, v.out + o.out, v.reasoning + o.reasoning, v.total + o.total}
}

func (v vec) json() string { return usage(v.in, v.cached, 0, v.out, v.reasoning, v.total) }

const (
	rollbackSession = "01a0aaa0-0000-7000-8000-000000000001"
	rollbackParent  = "01a0a000-0000-7000-8000-000000000002"
)

func rollbackMeta(at string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"forked_from_id":%q,"cwd":"/p","originator":"codex_vscode","source":"vscode"}}`,
		at, rollbackSession, rollbackParent)
}

// filler stands for the restored conversation text between the metadata and
// the first token_count; the scanner never stores it.
func filler(at string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","content":%q}}`, at, strings.Repeat("x", 2048))
}

func snapshot(at string, total, last vec) string { return tokenLine(at, total.json(), last.json()) }

// taskStarted is the new turn after an undo; its UUIDv7 id sorts after the
// session id, which the scanner treats as the end of a fork replay prefix.
func taskStarted(at, turn string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_started","turn_id":%q}}`, at, turn)
}

type rolloutFile struct {
	name  string
	lines []string
}

// rollbackScenario returns the files of one session and the usage they
// represent. Main conversation: states 900 (inherited from the parent), 1000,
// 1150, 1210. Undo back to 1150 is replayed twice; a request re-sent from
// 1150 and a request from 1210 with the same size are new usage.
func rollbackScenario() (files []rolloutFile, want int64) {
	s900, s1000, s1150, s1210 := state(900), state(1000), state(1150), state(1210)
	request := state(1230).sub(s1150) // 80 tokens
	main := rolloutFile{name: "rollout-2026-10-01T09-00-00-" + rollbackSession + ".jsonl", lines: []string{
		rollbackMeta("2026-10-01T01:00:00Z"),
		filler("2026-10-01T01:00:01Z"),
		snapshot("2026-10-01T01:00:02Z", s1000, s1000.sub(s900)),
		snapshot("2026-10-01T01:10:00Z", s1150, s1150.sub(s1000)),
		snapshot("2026-10-01T01:20:00Z", s1210, s1210.sub(s1150)),
	}}
	undo := rolloutFile{name: "rollout-2026-10-01T10-10-00-" + rollbackSession + "_01a1b000-0000-7000-8000-000000000003.jsonl", lines: []string{
		rollbackMeta("2026-10-01T02:10:00Z"),
		filler("2026-10-01T02:10:01Z"),
		taskStarted("2026-10-01T02:10:10Z", "01a1b000-0000-7000-8000-000000000003"),
		snapshot("2026-10-01T02:10:20Z", s1150, s1150.sub(s1000)),
	}}
	resend := rolloutFile{name: "rollout-2026-10-01T10-20-00-" + rollbackSession + "_01a1b100-0000-7000-8000-000000000004.jsonl", lines: []string{
		rollbackMeta("2026-10-01T02:20:00Z"),
		filler("2026-10-01T02:20:01Z"),
		taskStarted("2026-10-01T02:20:10Z", "01a1b100-0000-7000-8000-000000000004"),
		snapshot("2026-10-01T02:20:20Z", s1150.add(request), request),
	}}
	undoAgain := rolloutFile{name: "rollout-2026-10-01T10-30-00-" + rollbackSession + "_01a1b200-0000-7000-8000-000000000005.jsonl", lines: []string{
		rollbackMeta("2026-10-01T02:30:00Z"),
		filler("2026-10-01T02:30:01Z"),
		taskStarted("2026-10-01T02:30:10Z", "01a1b200-0000-7000-8000-000000000005"),
		snapshot("2026-10-01T02:30:20Z", s1150, s1150.sub(s1000)),
	}}
	sameSize := rolloutFile{name: "rollout-2026-10-01T10-40-00-" + rollbackSession + "_01a1b300-0000-7000-8000-000000000006.jsonl", lines: []string{
		rollbackMeta("2026-10-01T02:40:00Z"),
		filler("2026-10-01T02:40:01Z"),
		taskStarted("2026-10-01T02:40:10Z", "01a1b300-0000-7000-8000-000000000006"),
		snapshot("2026-10-01T02:40:20Z", s1210.add(request), request),
	}}
	// Counted: main 100+150+60, re-sent 80, same-size request 80.
	return []rolloutFile{main, undo, resend, undoAgain, sameSize}, 310 + 80 + 80
}

func writeRollout(t *testing.T, dir string, file rolloutFile, lines int) string {
	t.Helper()
	path := filepath.Join(dir, file.name)
	if err := os.WriteFile(path, []byte(strings.Join(file.lines[:lines], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type scanOutcome struct {
	total    int64
	warnings []string
}

func outcome(t *testing.T, st *store.Store) scanOutcome {
	t.Helper()
	summary, err := st.Summary(context.Background(), model.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := st.Warnings(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, w := range warnings {
		kinds = append(kinds, w.Kind)
	}
	sort.Strings(kinds)
	return scanOutcome{total: summary.Usage.Total, warnings: kinds}
}

func scanOnce(t *testing.T, st *store.Store, home string, rebuild bool) {
	t.Helper()
	if _, err := (&Scanner{Store: st}).Scan(context.Background(), []string{home}, rebuild); err != nil {
		t.Fatal(err)
	}
}

func newRollbackHome(t *testing.T) (home, dir string, st *store.Store) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, ".codex")
	dir = filepath.Join(home, "sessions", "2026", "11", "13")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return home, dir, st
}

func checkOutcome(t *testing.T, label string, got scanOutcome, want int64) {
	t.Helper()
	if got.total != want || len(got.warnings) != 0 {
		t.Fatalf("%s: total=%d warnings=%v, want total=%d and no warnings", label, got.total, got.warnings, want)
	}
}

// All files present, scanned once, then rescanned, reopened and rebuilt.
func TestRollbackFilesCountEachRequestOnce(t *testing.T) {
	files, want := rollbackScenario()
	home, dir, st := newRollbackHome(t)
	for _, file := range files {
		writeRollout(t, dir, file, len(file.lines))
	}
	scanOnce(t, st, home, false)
	checkOutcome(t, "first scan", outcome(t, st), want)
	scanOnce(t, st, home, false)
	checkOutcome(t, "rescan", outcome(t, st), want)
	scanOnce(t, st, home, true)
	checkOutcome(t, "rebuild", outcome(t, st), want)
}

// The service sees each undo file twice: once right after it is created and
// before its token_count, then again once the snapshot is written.
func TestRollbackFilesScannedBeforeTheirSnapshotArrives(t *testing.T) {
	files, want := rollbackScenario()
	home, dir, st := newRollbackHome(t)
	writeRollout(t, dir, files[0], len(files[0].lines))
	scanOnce(t, st, home, false)
	for _, file := range files[1:] {
		writeRollout(t, dir, file, len(file.lines)-1)
		scanOnce(t, st, home, false)
		writeRollout(t, dir, file, len(file.lines))
		scanOnce(t, st, home, false)
	}
	checkOutcome(t, "two-pass scans", outcome(t, st), want)
	// A restarted service has no in-memory state.
	scanOnce(t, st, home, false)
	checkOutcome(t, "after restart", outcome(t, st), want)
}

// Codex moves files to archived_sessions independently, and a plain path
// sort would read an archived continuation before the conversation it
// continues. Files are read in creation order wherever they are.
func TestRollbackFilesInEitherFolder(t *testing.T) {
	files, want := rollbackScenario()
	placements := [][]bool{ // true: the file is in archived_sessions
		{false, false, false, false, false},
		{false, true, true, true, true},
		{true, false, false, false, false},
		{false, true, false, true, false},
		{true, true, true, true, true},
	}
	for _, archived := range placements {
		t.Run(fmt.Sprint(archived), func(t *testing.T) {
			home, dir, st := newRollbackHome(t)
			archive := filepath.Join(home, "archived_sessions")
			if err := os.MkdirAll(archive, 0o700); err != nil {
				t.Fatal(err)
			}
			for index, file := range files {
				target := dir
				if archived[index] {
					target = archive
				}
				writeRollout(t, target, file, len(file.lines))
			}
			scanOnce(t, st, home, false)
			checkOutcome(t, "scan", outcome(t, st), want)
			scanOnce(t, st, home, true)
			checkOutcome(t, "rebuild", outcome(t, st), want)
		})
	}
}

// A subagent file whose earlier turns Codex compacted away starts with a
// counter that already holds that usage, and no earlier snapshot exists
// anywhere. Its whole final cumulative is its usage; counting only the first
// request would drop the compacted turns.
func TestCompactedSubagentKeepsItsWholeCumulative(t *testing.T) {
	const child, parent = "01a0ee00-0000-7000-8000-000000000010", "01a0ed00-0000-7000-8000-000000000011"
	turn := "01a0ee40-0000-7000-8000-000000000012" // a later turn of the subagent
	earlier := state(5000)                         // compacted turns
	first := state(5300)
	final := state(5700)
	file := rolloutFile{name: "rollout-2026-10-01T08-00-00-" + child + ".jsonl", lines: []string{
		fmt.Sprintf(`{"timestamp":"2026-10-01T00:00:00Z","type":"session_meta","payload":{"id":%q,"forked_from_id":%q,"cwd":"/p","source":{"subagent":{"thread_spawn":{"parent_thread_id":%q}}}}}`, child, parent, parent),
		taskStarted("2026-10-01T00:00:00Z", turn),
		fmt.Sprintf(`{"timestamp":"2026-10-01T00:00:00Z","type":"turn_context","payload":{"turn_id":%q,"model":"gpt-5.6-sol","cwd":"/p"}}`, turn),
		`{"timestamp":"2026-10-01T00:00:00Z","type":"compacted","payload":{"message":""}}`,
		snapshot("2026-10-01T00:00:00Z", first, first.sub(earlier)),
		snapshot("2026-10-01T00:00:00Z", final, final.sub(first)),
	}}
	home, dir, st := newRollbackHome(t)
	writeRollout(t, dir, file, len(file.lines))
	scanOnce(t, st, home, false)
	checkOutcome(t, "compacted subagent", outcome(t, st), final.total)
	scanOnce(t, st, home, true)
	checkOutcome(t, "compacted subagent rebuilt", outcome(t, st), final.total)
}

// A ledger read before session points existed has none for its files. The
// first file that needs them reads the session's earlier files once.
func TestRollbackAfterUpgradeBackfillsEarlierFiles(t *testing.T) {
	files, want := rollbackScenario()
	home, dir, st := newRollbackHome(t)
	writeRollout(t, dir, files[0], len(files[0].lines))
	scanOnce(t, st, home, false)
	forgetSessionPoints(t, st)
	for _, file := range files[1:] {
		writeRollout(t, dir, file, len(file.lines))
	}
	scanOnce(t, st, home, false)
	checkOutcome(t, "after upgrade", outcome(t, st), want)
}

// Two requests of exactly the same size sent from different states are two
// requests; only the same request ending at the same state is a replay.
func TestRollbackSameSizeRequestsFromDifferentStatesBothCount(t *testing.T) {
	s900, s1000, s1150 := state(900), state(1000), state(1150)
	request := state(1230).sub(s1150)
	main := rolloutFile{name: "rollout-2026-10-01T09-00-00-" + rollbackSession + ".jsonl", lines: []string{
		rollbackMeta("2026-10-01T01:00:00Z"),
		snapshot("2026-10-01T01:00:02Z", s1000, s1000.sub(s900)),
		snapshot("2026-10-01T01:10:00Z", s1150, s1150.sub(s1000)),
		snapshot("2026-10-01T01:20:00Z", s1150.add(request), request),
	}}
	// After undoing the last request, the same prompt is sent from 1000 and
	// happens to use exactly as many tokens.
	again := rolloutFile{name: "rollout-2026-10-01T10-00-00-" + rollbackSession + "_01a1d2f1-0000-7000-8000-000000000002.jsonl", lines: []string{
		rollbackMeta("2026-10-01T02:00:00Z"),
		taskStarted("2026-10-01T02:00:05Z", "01a1d2f1-0000-7000-8000-000000000002"),
		snapshot("2026-10-01T02:00:20Z", s1000.add(request), request),
	}}
	home, dir, st := newRollbackHome(t)
	writeRollout(t, dir, main, len(main.lines))
	writeRollout(t, dir, again, len(again.lines))
	scanOnce(t, st, home, false)
	checkOutcome(t, "same-size requests", outcome(t, st), 100+150+80+80)
}

// An ordinary single conversation is unaffected: every increment counts once
// and rescans add nothing.
func TestOrdinaryConversationStillCountsEachIncrement(t *testing.T) {
	file := rolloutFile{name: "rollout-2026-10-01T08-00-00-01a0dd00-0000-7000-8000-000000000003.jsonl", lines: []string{
		`{"timestamp":"2026-10-01T00:00:00Z","type":"session_meta","payload":{"id":"01a0dd00-0000-7000-8000-000000000003","cwd":"/p","originator":"codex_cli_rs"}}`,
		snapshot("2026-10-01T00:00:01Z", state(100), state(100)),
		snapshot("2026-10-01T00:00:02Z", state(250), state(250).sub(state(100))),
		snapshot("2026-10-01T00:00:03Z", state(250), state(250).sub(state(100))),
		snapshot("2026-10-01T00:00:04Z", state(400), state(400).sub(state(250))),
	}}
	home, dir, st := newRollbackHome(t)
	writeRollout(t, dir, file, len(file.lines))
	scanOnce(t, st, home, false)
	scanOnce(t, st, home, false)
	checkOutcome(t, "ordinary conversation", outcome(t, st), 400)
	scanOnce(t, st, home, true)
	checkOutcome(t, "ordinary conversation rebuilt", outcome(t, st), 400)
}

func forgetSessionPoints(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.ForgetSessionPointsForTest(context.Background()); err != nil {
		t.Fatal(err)
	}
}
