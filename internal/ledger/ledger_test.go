package ledger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tokenusage/internal/model"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openClaudeFixture(t *testing.T, lines ...string) *Provider {
	t.Helper()
	home := t.TempDir()
	writeLines(t, filepath.Join(home, "projects", "demo", "session-a.jsonl"), lines...)
	p, err := OpenClaude(t.TempDir(), []string{home})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	if err := p.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}

func claudeLine(id, request, timestamp, extra string) string {
	return `{"type":"assistant","sessionId":"s1","uuid":"u-` + id + `","requestId":"` + request + `","timestamp":"` + timestamp + `"` + extra +
		`,"message":{"id":"msg_` + id + `","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_read_input_tokens":1000,` +
		`"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":100},` +
		`"output_tokens":50,"output_tokens_details":{"thinking_tokens":20}}}}`
}

func TestClaudeDefaultsAndPricing(t *testing.T) {
	// No speed, region or tier fields: they default to standard / global.
	p := openClaudeFixture(t, claudeLine("a", "req_a", "2026-10-01T00:00:00Z", ""))
	entries := p.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	want := model.TokenUsage{Input: 1110, CachedInput: 1000, CacheWriteInput: 100, Output: 50, ReasoningOutput: 20, Total: 1160}
	if e.Usage != want {
		t.Fatalf("usage = %+v, want %+v", e.Usage, want)
	}
	// 10×4 + 1000×0.2 + 50×20 + 100×8 USD per million = $0.00204.
	cost := e.Price.RegularNano + e.Price.CachedNano + e.Price.CacheWriteNano + e.Price.OutputNano
	if !e.Price.Priced || cost != 2_040_000 {
		t.Fatalf("price = %+v (nano %d)", e.Price, cost)
	}
}

func TestClaudeUnverifiedDateStaysUnpriced(t *testing.T) {
	p := openClaudeFixture(t, claudeLine("a", "req_a", "2026-09-01T00:00:00Z", ""))
	if e := p.Entries()[0]; e.Price.Priced || e.Price.ReasonKind != "unverified_rate" {
		t.Fatalf("price before the effective date = %+v", e.Price)
	}
}

func TestClaudeOtherModelsPricedAndSyntheticSkipped(t *testing.T) {
	fable := strings.Replace(claudeLine("a", "req_a", "2026-10-01T00:00:00Z", ""), "claude-opus-5-5", "claude-fable-5-1", 1)
	synthetic := strings.Replace(claudeLine("b", "req_b", "2026-10-01T00:00:01Z", ""), "claude-opus-5-5", "<synthetic>", 1)
	entries := openClaudeFixture(t, fable, synthetic).Entries()
	if len(entries) != 1 || entries[0].Model != "claude-fable-5-1" {
		t.Fatalf("entries = %+v", entries)
	}
	// 10×10 + 1000×0.25 + 50×50 + 100×20 USD per million = $0.00485.
	e := entries[0]
	cost := e.Price.RegularNano + e.Price.CachedNano + e.Price.CacheWriteNano + e.Price.OutputNano
	if !e.Price.Priced || cost != 4_850_000 {
		t.Fatalf("price = %+v (nano %d)", e.Price, cost)
	}
}

func TestClaudeDeduplicatesRepeatedBlocksAndSidechainReplays(t *testing.T) {
	p := openClaudeFixture(t,
		claudeLine("a", "req_a", "2026-10-01T00:00:00Z", ""),
		claudeLine("a", "req_a", "2026-10-01T00:00:01Z", ""),
		claudeLine("a", "req_a", "2026-10-01T00:00:02Z", `,"isSidechain":true`),
		claudeLine("b", "req_b", "2026-10-01T00:01:00Z", ""),
	)
	if got := len(p.Entries()); got != 2 {
		t.Fatalf("entries = %d, want 2", got)
	}
}

func TestClaudeRescanIsIdempotent(t *testing.T) {
	p := openClaudeFixture(t, claudeLine("a", "req_a", "2026-10-01T00:00:00Z", ""))
	before, err := p.ledger.records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := p.ledger.records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || len(after) != 1 || before[0].Payload != after[0].Payload {
		t.Fatalf("rescan changed the ledger:\n%v\n%v", before, after)
	}
}

func TestClaudeKeepsHistoryWhenSourceIsDeleted(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "projects", "demo", "session-a.jsonl")
	writeLines(t, file, claudeLine("a", "req_a", "2026-10-01T00:00:00Z", ""))
	p, err := OpenClaude(t.TempDir(), []string{home})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := p.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.Entries()) != 1 {
		t.Fatal("deleting the source removed saved usage")
	}
}

func openAntigravityFixture(t *testing.T, lines ...string) *Provider {
	t.Helper()
	home := t.TempDir()
	writeLines(t, filepath.Join(home, "antigravity", "brain", "conv-1", ".system_generated", "logs", "transcript.jsonl"), lines...)
	p, err := OpenAntigravity(t.TempDir(), home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	if err := p.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAntigravityTranscriptCounters(t *testing.T) {
	p := openAntigravityFixture(t,
		"{\"type\":\"USER_SETTINGS_CHANGE\",\"content\":\"The user changed setting `Model Selection` from A to Gemini 3.1 Pro (High).\",\"created_at\":\"2026-10-01T00:00:00Z\"}",
		`{"type":"USER_INPUT","content":"hi","created_at":"2026-10-01T00:00:01Z"}`,
		`{"type":"PLANNER_RESPONSE","step_index":2,"created_at":"2026-10-01T00:00:02Z","input_tokens":1000,"cache_read_tokens":4000,"output_tokens":200,"content":"ok"}`,
	)
	entries := p.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	if e.Model != "gemini-3.1-pro" || e.Precision != "recorded" || e.TokenSource != "transcript-counters" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Usage.Total != 5200 || e.Usage.CachedInput != 4000 {
		t.Fatalf("usage = %+v", e.Usage)
	}
	// 1000×2 + 4000×0.2 + 200×12 USD per million.
	if cost := e.Price.RegularNano + e.Price.CachedNano + e.Price.OutputNano; !e.Price.Priced || cost != 5_200_000 {
		t.Fatalf("price = %+v", e.Price)
	}
}

func TestAntigravityTextEstimate(t *testing.T) {
	p := openAntigravityFixture(t,
		`{"type":"USER_INPUT","content":"12345678","created_at":"2026-10-01T00:00:00Z"}`,
		`{"type":"PLANNER_RESPONSE","step_index":1,"created_at":"2026-10-01T00:00:01Z","content":"中文abcd","thinking":"abcdefgh"}`,
	)
	e := p.Entries()[0]
	// Context: "12345678" = 2 tokens. Output: 2 CJK + ceil(4/4) = 3. Thinking: 2.
	if e.Precision != "estimated" || e.Usage.Input != 2 || e.Usage.Output != 5 || e.Usage.ReasoningOutput != 2 {
		t.Fatalf("estimate = %+v", e)
	}
	if e.Model != canonicalModel("antigravity-unknown") {
		t.Fatalf("model = %q", e.Model)
	}
}

func TestAntigravityRoutingModelNeedsExplicitChoice(t *testing.T) {
	reported := "gemini-pro-default"
	record := AntigravityRecord{Model: "gemini-3.1-pro", ReportedModel: &reported, Totals: agTotals{Input: 10, Total: 10}}
	if price := priceAntigravity(record, mustDate("2026-10-01T00:00:00Z"), nil); price.Priced || price.ReasonKind != "ambiguous_routing_model" {
		t.Fatalf("price = %+v", price)
	}
	selected := "Gemini 3.1 Pro (High)"
	record.SelectedModel = &selected
	if price := priceAntigravity(record, mustDate("2026-10-01T00:00:00Z"), nil); !price.Priced {
		t.Fatalf("explicit choice should be priced: %+v", price)
	}
}

func TestProtoUsageStats(t *testing.T) {
	// field 1 (model code) = 1322, 2 (input) = 100, 3 (total output) = 30,
	// 5 (cache read) = 400, 9 (thinking) = 10, 10 (response) = 20, 11 = "r1".
	buf := []byte{0x08, 0xaa, 0x0a, 0x10, 100, 0x18, 30, 0x28, 0x90, 0x03, 0x48, 10, 0x50, 20, 0x5a, 2, 'r', '1'}
	usage, err := agUsageStats(buf)
	if err != nil || usage == nil {
		t.Fatalf("usage = %v, err = %v", usage, err)
	}
	if *usage.ModelCode != 1322 || usage.Uncached != 100 || usage.Cached != 400 || usage.Reasoning != 10 ||
		usage.TextOutput != 20 || usage.OutputSplit != "split" || *usage.ResponseID != "r1" {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestJSStringifyKeepsOrderAndNumberFormat(t *testing.T) {
	value, err := parseJS([]byte(`{"b":1.50,"a":[1e21,1e-7,0.000001,"é<"],"c":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := value.stringify(), `{"b":1.5,"a":[1e+21,1e-7,0.000001,"é<"],"c":null}`; got != want {
		t.Fatalf("stringify = %s, want %s", got, want)
	}
	var missing jsValue
	if missing.truthy() {
		t.Fatal("a missing field must be falsy")
	}
}

func TestModelNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"Gemini 3.1 Pro (High)":    "gemini-3.1-pro",
		"Gemini 3.8 Flash Tiered":  "gemini-3.8-flash",
		"Claude Opus 5.5 Thinking": "claude-opus-5.5",
		"Mystery":                  "antigravity-mystery",
	} {
		if got := normalizeAGModel(in); got != want {
			t.Errorf("normalizeAGModel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := canonicalModel("claude-opus-5.5"); got != "claude-opus-5-5" {
		t.Errorf("canonicalModel = %q", got)
	}
	if got := claudeCanonical("claude-sonnet-5.5-20261007"); got != "claude-sonnet-5-5" {
		t.Errorf("claudeCanonical = %q", got)
	}
}

func TestSelectFiltersBySourceAndDate(t *testing.T) {
	p := openClaudeFixture(t,
		claudeLine("a", "req_a", "2026-10-01T00:00:00Z", ""),
		claudeLine("b", "req_b", "2026-10-02T00:00:00Z", ""),
	)
	loc := taipei
	if got := len(p.Select(model.Filter{SinceDate: "2026-10-02"}, loc)); got != 1 {
		t.Fatalf("since filter = %d", got)
	}
	if got := len(p.Select(model.Filter{Source: "Codex Desktop"}, loc)); got != 0 {
		t.Fatalf("source filter = %d", got)
	}
	if got := len(p.Select(model.Filter{Project: "/any"}, loc)); got != 0 {
		t.Fatalf("project filter = %d", got)
	}
	encoded, _ := json.Marshal(Summary(p.Select(model.Filter{}, loc)))
	if !strings.Contains(string(encoded), `"event_count":2`) {
		t.Fatalf("summary = %s", encoded)
	}
}
