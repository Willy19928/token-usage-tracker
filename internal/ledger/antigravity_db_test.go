package ledger

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pbVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func pbUint(num, v uint64) []byte { return append(pbVarint(num<<3), pbVarint(v)...) }

func pbBytes(num uint64, b []byte) []byte {
	return append(append(pbVarint(num<<3|2), pbVarint(uint64(len(b)))...), b...)
}

func pbJoin(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// agTestUsage is a ModelUsageStats message: input, cache read, thinking and
// response output.
func agTestUsage(input, cached, thinking, response uint64) []byte {
	return pbJoin(pbUint(2, input), pbUint(3, thinking+response), pbUint(5, cached), pbUint(9, thinking), pbUint(10, response))
}

type agTestStep struct {
	idx          int64
	at           time.Time
	model, label string
	genUsage     []byte // usage in the generation metadata
	stepUsage    []byte // usage in the step's own metadata
	noGenMeta    bool
}

// writeAntigravityDB writes a conversation database with one planner
// response per step.
func writeAntigravityDB(t *testing.T, path string, steps ...agTestStep) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteURI(path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE gen_metadata(idx INTEGER PRIMARY KEY, data BLOB);
		CREATE TABLE steps(idx INTEGER PRIMARY KEY, step_type INTEGER, metadata BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i, step := range steps {
		metadata := pbBytes(1, pbJoin(pbUint(1, uint64(step.at.Unix())), pbUint(2, uint64(step.at.Nanosecond()))))
		if step.stepUsage != nil {
			metadata = append(metadata, pbBytes(9, step.stepUsage)...)
		}
		if _, err := db.Exec(`INSERT INTO steps VALUES(?,15,?)`, step.idx, metadata); err != nil {
			t.Fatal(err)
		}
		if step.noGenMeta {
			continue
		}
		inner := pbJoin(pbBytes(19, []byte(step.model)),
			pbBytes(20, pbJoin(pbBytes(1, []byte("last_step_index")), pbBytes(2, []byte(strconv.FormatInt(step.idx-1, 10))))))
		if step.label != "" {
			inner = append(inner, pbBytes(21, []byte(step.label))...)
		}
		if step.genUsage != nil {
			inner = append(inner, pbBytes(4, step.genUsage)...)
		}
		if _, err := db.Exec(`INSERT INTO gen_metadata VALUES(?,?)`, i, pbBytes(1, inner)); err != nil {
			t.Fatal(err)
		}
	}
}

func openAntigravityHome(t *testing.T, home string) *Provider {
	t.Helper()
	p, err := OpenAntigravity(t.TempDir(), home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// antigravityEntries returns the saved requests by step.
func antigravityEntries(t *testing.T, p *Provider) map[string]Entry {
	t.Helper()
	if err := p.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := map[string]Entry{}
	for _, e := range p.Entries() {
		out[e.ID[strings.LastIndex(e.ID, ":")+1:]] = e
	}
	return out
}

func agTranscriptPath(home, id string) string {
	return filepath.Join(home, "antigravity", "brain", id, ".system_generated", "logs", "transcript.jsonl")
}

// A conversation run by another agent has a database but no transcript.
func TestAntigravityReadsConversationWithoutTranscript(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 11, 15, 1, 2, 3, 456_000_000, time.UTC)
	writeAntigravityDB(t, filepath.Join(home, "antigravity", "conversations", "worker-1.db"),
		agTestStep{idx: 1, at: at, model: "gemini-4-argon-s", label: "Gemini 4 Argon (High)", genUsage: agTestUsage(100, 1000, 20, 30)},
		agTestStep{idx: 2, at: at.Add(time.Second), noGenMeta: true},
		agTestStep{idx: 3, at: at.Add(2 * time.Second), noGenMeta: true, stepUsage: agTestUsage(50, 2000, 0, 10)},
	)
	entries := antigravityEntries(t, openAntigravityHome(t, home))
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	first, third := entries["1"], entries["3"]
	if first.Model != "gemini-4-argon" || first.Usage.Total != 1150 || first.Usage.ReasoningOutput != 20 ||
		first.TokenSource != "gen-metadata" || !first.Timestamp.Equal(at) || !first.Price.Priced {
		t.Fatalf("first = %+v", first)
	}
	// The step names no model, so the conversation's model carries on.
	if third.Model != "gemini-4-argon" || third.Usage.Total != 2060 || third.TokenSource != "steps-metadata" {
		t.Fatalf("third = %+v", third)
	}
	if first.Session != hashText("worker-1") || third.Session != first.Session {
		t.Fatalf("sessions = %q, %q", first.Session, third.Session)
	}
}

// Antigravity trims the start of a long transcript; the database still holds
// those steps. A step read from the transcript keeps the model it showed.
func TestAntigravityFillsStepsTrimmedFromTranscript(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	writeAntigravityDB(t, filepath.Join(home, "antigravity", "conversations", "conv-1.db"),
		agTestStep{idx: 1, at: at, model: "gemini-4-argon-trial", genUsage: agTestUsage(100, 0, 0, 10)},
		agTestStep{idx: 3, at: at.Add(time.Minute), model: "gemini-pro-default", genUsage: agTestUsage(200, 0, 0, 20)},
	)
	transcript := agTranscriptPath(home, "conv-1")
	writeLines(t, transcript,
		"{\"type\":\"USER_INPUT\",\"content\":\"The user changed setting `Model Selection` from A to Gemini 3.1 Pro (High).\",\"created_at\":\"2026-11-15T00:00:30Z\"}",
		`{"type":"PLANNER_RESPONSE","step_index":3,"created_at":"2026-11-15T00:01:00Z","content":"ok"}`,
	)
	p := openAntigravityHome(t, home)
	entries := antigravityEntries(t, p)
	if len(entries) != 2 || entries["1"].Model != "gemini-4-argon" || entries["1"].Usage.Total != 110 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries["3"].Model != "gemini-3.1-pro" || !entries["3"].Price.Priced || entries["3"].Usage.Total != 220 {
		t.Fatalf("step 3 = %+v", entries["3"])
	}
	// The database alone cannot name step 3's model.
	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}
	if got := antigravityEntries(t, p)["3"]; got.Model != "gemini-3.1-pro" || !got.Price.Priced {
		t.Fatalf("step 3 once the transcript is gone = %+v", got)
	}
}

func TestAntigravityRescanSkipsUnchangedConversations(t *testing.T) {
	home := t.TempDir()
	transcript := agTranscriptPath(home, "conv-1")
	writeLines(t, transcript,
		`{"type":"PLANNER_RESPONSE","step_index":1,"created_at":"2026-11-15T00:00:00Z","input_tokens":10,"cache_read_tokens":0,"output_tokens":5}`)
	writeAntigravityDB(t, filepath.Join(home, "antigravity", "conversations", "conv-2.db"),
		agTestStep{idx: 1, at: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), model: "gemini-3.8-flash-n", genUsage: agTestUsage(10, 0, 0, 5)})
	p := openAntigravityHome(t, home)
	ctx := context.Background()
	scan := func() agScan {
		t.Helper()
		result, err := scanAntigravity(ctx, p.ledger, home)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if s := scan(); s.FilesProcessed != 2 || s.UnchangedSources != 0 {
		t.Fatalf("first scan = %+v", s)
	}
	if s := scan(); s.FilesProcessed != 0 || s.UnchangedSources != 2 {
		t.Fatalf("unchanged rescan = %+v", s)
	}
	file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"PLANNER_RESPONSE","step_index":2,"created_at":"2026-11-15T00:01:00Z","input_tokens":20,"cache_read_tokens":0,"output_tokens":5}` + "\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if s := scan(); s.FilesProcessed != 1 || s.UnchangedSources != 1 || s.Observations != 2 {
		t.Fatalf("rescan after a change = %+v", s)
	}
	if err := p.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(p.Entries()); n != 3 {
		t.Fatalf("entries = %d", n)
	}
}

func agPlannerLine(step int, at string, input, output int) string {
	return `{"type":"PLANNER_RESPONSE","step_index":` + strconv.Itoa(step) + `,"created_at":"` + at +
		`","input_tokens":` + strconv.Itoa(input) + `,"cache_read_tokens":0,"output_tokens":` + strconv.Itoa(output) + `,"content":"ok"}`
}

// agEntryTotals returns the saved requests' token totals in time order.
func agEntryTotals(t *testing.T, p *Provider) []int64 {
	t.Helper()
	if err := p.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, e := range p.Entries() {
		out = append(out, e.Usage.Total)
	}
	return out
}

func agSameTotals(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// After a rewind to a checkpoint Antigravity reuses step indexes, and its
// database keeps only the latest use. Every use is a request of its own.
func TestAntigravityCountsEveryUseOfAReusedStepIndex(t *testing.T) {
	home := t.TempDir()
	db := filepath.Join(home, "antigravity", "conversations", "conv-1.db")
	at := func(minute int) time.Time { return time.Date(2026, 11, 19, 0, minute, 0, 0, time.UTC) }
	stamp := func(minute int) string { return at(minute).Format(time.RFC3339) }
	writeAntigravityDB(t, db,
		agTestStep{idx: 1, at: at(0), model: "gemini-4-neon", genUsage: agTestUsage(100, 0, 0, 10)},
		agTestStep{idx: 2, at: at(3), model: "gemini-4-neon", genUsage: agTestUsage(400, 0, 0, 40)},
		agTestStep{idx: 3, at: at(4), model: "gemini-4-neon", genUsage: agTestUsage(500, 0, 0, 50)},
	)
	transcript := agTranscriptPath(home, "conv-1")
	lines := []string{
		agPlannerLine(1, stamp(0), 100, 10),
		agPlannerLine(2, stamp(1), 200, 20),
		agPlannerLine(3, stamp(2), 300, 30),
		// Rewound to step 1 and continued.
		agPlannerLine(2, stamp(3), 400, 40),
		agPlannerLine(3, stamp(4), 500, 50),
	}
	writeLines(t, transcript, lines...)
	p := openAntigravityHome(t, home)
	if got := agEntryTotals(t, p); !agSameTotals(got, 110, 220, 330, 440, 550) {
		t.Fatalf("totals = %v", got)
	}
	for _, e := range p.Entries() {
		if !e.Price.Priced || e.Model != "gemini-4-neon" {
			t.Fatalf("entry = %+v", e)
		}
	}

	// Rewound again: the use of step 3 that was latest becomes an earlier one.
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	writeAntigravityDB(t, db,
		agTestStep{idx: 1, at: at(0), model: "gemini-4-neon", genUsage: agTestUsage(100, 0, 0, 10)},
		agTestStep{idx: 2, at: at(3), model: "gemini-4-neon", genUsage: agTestUsage(400, 0, 0, 40)},
		agTestStep{idx: 3, at: at(5), model: "gemini-4-neon", genUsage: agTestUsage(600, 0, 0, 60)},
	)
	lines = append(lines, agPlannerLine(3, stamp(5), 600, 60))
	writeLines(t, transcript, lines...)
	if got := agEntryTotals(t, p); !agSameTotals(got, 110, 220, 330, 440, 550, 660) {
		t.Fatalf("totals after a second rewind = %v", got)
	}

	// Antigravity trims the start of the transcript; the saved requests stay.
	writeLines(t, transcript, lines[3:]...)
	if got := agEntryTotals(t, p); !agSameTotals(got, 110, 220, 330, 440, 550, 660) {
		t.Fatalf("totals once the transcript is trimmed = %v", got)
	}
}

// The CLI numbers each turn of a conversation from zero, and its database
// keeps only the last turn.
func TestAntigravityCountsEveryCLITurn(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 11, 15, 0, 0, 30, 0, time.UTC)
	writeAntigravityDB(t, filepath.Join(home, "antigravity-cli", "conversations", "conv-1.db"),
		agTestStep{idx: 1, at: at, model: "gemini-4-neon", genUsage: agTestUsage(1002, 0, 0, 7)})
	transcript := filepath.Join(home, "antigravity-cli", "brain", "conv-1", ".system_generated", "logs", "transcript.jsonl")
	writeLines(t, transcript,
		`{"type":"USER_INPUT","step_index":0,"created_at":"2026-11-15T00:00:00Z","content":"hi"}`,
		agPlannerLine(1, "2026-11-15T00:00:01Z", 1000, 5),
		`{"type":"USER_INPUT","step_index":0,"created_at":"2026-11-15T00:00:29Z","content":"hi again"}`,
		agPlannerLine(1, "2026-11-15T00:00:30Z", 1002, 7),
		// The same response written twice is one request.
		agPlannerLine(1, "2026-11-15T00:00:30Z", 1002, 7),
	)
	p := openAntigravityHome(t, home)
	got := agEntryTotals(t, p)
	if !agSameTotals(got, 1005, 1009) {
		t.Fatalf("totals = %v", got)
	}
	if e := p.Entries()[1]; e.TokenSource != "gen-metadata" || e.Model != "gemini-4-neon" {
		t.Fatalf("last turn = %+v", e)
	}
}
