package usage

import (
	"fmt"
	"testing"
)

// Conversations written with token_usage_record count each request from
// that record, and their token_count snapshots carry the session-wide
// cumulative. An undo or restore then writes a file with a new turn whose
// only token_count replays an old snapshot; that turn has no record of its own.

const (
	respSession = "01a0bbb0-0000-7000-8000-000000000001"
	respParent  = "01a0b000-0000-7000-8000-000000000002"
)

func respMeta(at string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"forked_from_id":%q,"cwd":"/p","originator":"codex_vscode","source":"vscode"}}`,
		at, respSession, respParent)
}

func respTurn(at, turn string) []string {
	return []string{
		taskStarted(at, turn),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"turn_id":%q,"model":"gpt-5.6-sol","cwd":"/p"}}`, at, turn),
	}
}

// respRequest is one request: its record (turn counter) and the snapshot
// with the session-wide cumulative after it.
func respRequest(at, turn, id string, request, turnTotal, session vec) []string {
	return []string{
		responseLine(respSession, turn, id, at, request.json(), turnTotal.json()),
		snapshot(at, session, request),
	}
}

func lines(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// responseRollbackScenario: the main file makes requests r1 and r2 in turn
// T1. Undo files replay the state after r2 and after r1 (twice, at new
// times, under new turns). A request sent after undoing r2 is new usage.
func responseRollbackScenario() (files []rolloutFile, want int64) {
	base := state(50000) // the conversation's earlier history
	r1, r2, r5 := state(300).sub(state(0)), state(320).sub(state(0)), state(310).sub(state(0))
	s1 := base.add(r1)
	s2 := s1.add(r2)
	s5 := s1.add(r5)
	t1, t2, t3, t4, t5 := "01a1c100-0000-7000-8000-000000000011", "01a1c200-0000-7000-8000-000000000012",
		"01a1c300-0000-7000-8000-000000000013", "01a1c400-0000-7000-8000-000000000014", "01a1c500-0000-7000-8000-000000000015"
	main := rolloutFile{name: "rollout-2026-10-02T08-00-00-" + respSession + ".jsonl", lines: lines(
		[]string{respMeta("2026-10-02T00:00:00Z")},
		respTurn("2026-10-02T00:00:01Z", t1),
		respRequest("2026-10-02T00:00:02Z", t1, "resp-1", r1, r1, s1),
		[]string{snapshot("2026-10-02T00:00:03Z", s1, r1)}, // repeated notice
		respRequest("2026-10-02T00:00:04Z", t1, "resp-2", r2, r1.add(r2), s2),
	)}
	replayS2 := rolloutFile{name: "rollout-2026-10-02T09-00-00-" + respSession + "_01a1c200-0000-7000-8000-000000000012.jsonl", lines: lines(
		[]string{respMeta("2026-10-02T01:00:00Z"), filler("2026-10-02T01:00:01Z")},
		respTurn("2026-10-02T01:00:02Z", t2),
		[]string{snapshot("2026-10-02T01:00:03Z", s2, r2), snapshot("2026-10-02T01:00:04Z", s2, r2)},
	)}
	replayS1 := rolloutFile{name: "rollout-2026-10-02T09-10-00-" + respSession + "_01a1c300-0000-7000-8000-000000000013.jsonl", lines: lines(
		[]string{respMeta("2026-10-02T01:10:00Z"), filler("2026-10-02T01:10:01Z")},
		respTurn("2026-10-02T01:10:02Z", t3),
		[]string{snapshot("2026-10-02T01:10:03Z", s1, r1)},
	)}
	replayS1Again := rolloutFile{name: "rollout-2026-10-02T09-20-00-" + respSession + "_01a1c400-0000-7000-8000-000000000014.jsonl", lines: lines(
		[]string{respMeta("2026-10-02T01:20:00Z"), filler("2026-10-02T01:20:01Z")},
		respTurn("2026-10-02T01:20:02Z", t4),
		[]string{snapshot("2026-10-02T01:20:03Z", s1, r1)},
	)}
	resent := rolloutFile{name: "rollout-2026-10-02T09-30-00-" + respSession + "_01a1c500-0000-7000-8000-000000000015.jsonl", lines: lines(
		[]string{respMeta("2026-10-02T01:30:00Z"), filler("2026-10-02T01:30:01Z")},
		respTurn("2026-10-02T01:30:02Z", t5),
		respRequest("2026-10-02T01:30:03Z", t5, "resp-5", r5, r5, s5),
	)}
	return []rolloutFile{main, replayS2, replayS1, replayS1Again, resent}, r1.total + r2.total + r5.total
}

func TestResponseRollbackReplaysAreNotCountedAgain(t *testing.T) {
	files, want := responseRollbackScenario()
	home, dir, st := newRollbackHome(t)
	for _, file := range files {
		writeRollout(t, dir, file, len(file.lines))
	}
	scanOnce(t, st, home, false)
	checkOutcome(t, "scan", outcome(t, st), want)
	scanOnce(t, st, home, true)
	checkOutcome(t, "rebuild", outcome(t, st), want)
}

// The service usually reads each undo file before and after its snapshot.
func TestResponseRollbackReplaysReadInTwoPasses(t *testing.T) {
	files, want := responseRollbackScenario()
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
}

// Session points are backfilled once per session, at its first undo. A
// request recorded after that, in a turn counted from its record, must still
// be remembered: a later undo back to the state after it is not new usage.
func TestResponseRollbackAfterBackfillIsNotCountedAgain(t *testing.T) {
	files, want := responseRollbackScenario()
	main, replayS1, resent := files[0], files[2], files[4]
	base, r1, r5, r6 := state(50000), state(300), state(310), state(330)
	s5 := base.add(r1).add(r5)
	t5, t6 := "01a1c500-0000-7000-8000-000000000015", "01a1c600-0000-7000-8000-000000000016"
	// The conversation goes on after the re-sent request, then is undone back
	// to the state after it.
	resent.lines = append(append([]string{}, resent.lines...),
		respRequest("2026-10-02T01:30:04Z", t5, "resp-6", r6, r5.add(r6), s5.add(r6))...)
	want += r6.total
	replayS5 := rolloutFile{name: "rollout-2026-10-02T09-40-00-" + respSession + "_" + t6 + ".jsonl", lines: lines(
		[]string{respMeta("2026-10-02T01:40:00Z"), filler("2026-10-02T01:40:01Z")},
		respTurn("2026-10-02T01:40:02Z", t6),
		[]string{snapshot("2026-10-02T01:40:03Z", s5, r5)},
	)}
	home, dir, st := newRollbackHome(t)
	for _, file := range []rolloutFile{main, replayS1, resent, replayS5} {
		writeRollout(t, dir, file, len(file.lines))
		scanOnce(t, st, home, false)
	}
	checkOutcome(t, "after backfill", outcome(t, st), want)
	scanOnce(t, st, home, true)
	checkOutcome(t, "rebuild", outcome(t, st), want)
}
