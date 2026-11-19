package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenusage/internal/model"
)

// Antigravity transcript and conversation-database parser. Each scan
// re-reads every live transcript and its conversation database; saved records
// are never removed, so sessions whose sources were deleted keep their history.

const antigravitySchema = `PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS records(id TEXT PRIMARY KEY, payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS state(id TEXT PRIMARY KEY, payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS record_revisions(id TEXT NOT NULL,old_hash TEXT NOT NULL,old_payload TEXT NOT NULL,new_payload TEXT NOT NULL,reason TEXT NOT NULL,changed_at TEXT NOT NULL,PRIMARY KEY(id,old_hash));
CREATE TRIGGER IF NOT EXISTS records_revision_guard_update BEFORE UPDATE ON records
  WHEN coalesce(json_extract(OLD.payload,'$.accountingRevision'),0)>=3 AND coalesce(json_extract(NEW.payload,'$.accountingRevision'),0)<3 BEGIN SELECT RAISE(IGNORE); END;
CREATE TRIGGER IF NOT EXISTS records_revision_guard_insert BEFORE INSERT ON records
  WHEN EXISTS(SELECT 1 FROM state WHERE id='accounting-revision' AND payload='3') AND coalesce(json_extract(NEW.payload,'$.accountingRevision'),0)<3 BEGIN SELECT RAISE(IGNORE); END;`

const antigravityAccountingRevision = 3

type agTotals struct {
	Input             int64   `json:"input_tokens"`
	Cached            int64   `json:"cached_input_tokens"`
	CacheCreation     int64   `json:"cache_creation_input_tokens"`
	Output            int64   `json:"output_tokens"`
	Reasoning         int64   `json:"reasoning_output_tokens"`
	Total             int64   `json:"total_tokens"`
	BillableTotal     int64   `json:"billable_total_tokens"`
	TotalCostUSD      float64 `json:"total_cost_usd"`
	ConversationCount int64   `json:"conversation_count"`
}

type AntigravityRecord struct {
	Line               int64           `json:"line"`
	StepIndex          *int64          `json:"stepIndex,omitempty"`
	ResponseID         *string         `json:"responseId"`
	Timestamp          string          `json:"timestamp"`
	Model              string          `json:"model"`
	SelectedModel      *string         `json:"selectedModel"`
	ReportedModel      *string         `json:"reportedModel"`
	Precision          string          `json:"precision"`
	TokenSource        string          `json:"tokenSource"`
	ModelCode          *int64          `json:"modelCode"`
	OutputSplit        string          `json:"outputSplit"`
	Totals             agTotals        `json:"totals"`
	Session            string          `json:"session"`
	AccountingRevision int64           `json:"accountingRevision"`
	RecoveryEvidence   json.RawMessage `json:"recoveryEvidence,omitempty"`
	// Source is agSourceDatabase for a step read from its conversation
	// database alone; it is empty for one read from its transcript.
	Source string `json:"source,omitempty"`
	// Superseded is set on a planner response whose step index the
	// conversation later reused, to the response's own time. Antigravity
	// reuses step indexes after a rewind to a checkpoint, and the CLI numbers
	// each turn from zero; the database keeps only the latest use.
	Superseded string `json:"superseded,omitempty"`
}

const agSourceDatabase = "database"

// key identifies a step. The latest use of a step index keeps the plain key,
// so trimming the start of a transcript never changes it; an earlier use is
// told apart by its time.
func (r AntigravityRecord) key() string {
	id := "step-v2:" + r.Session + ":" + strconv.FormatInt(r.Line, 10)
	if r.StepIndex != nil {
		id = "step-v2:" + r.Session + ":" + strconv.FormatInt(*r.StepIndex, 10)
	}
	if r.Superseded != "" {
		id += "@" + r.Superseded
	}
	return id
}

func OpenAntigravity(directory, geminiHome string) (*Provider, error) {
	db, err := openLedger(directory, antigravitySchema)
	if err != nil {
		return nil, err
	}
	if geminiHome == "" {
		home := os.Getenv("USERPROFILE")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		geminiHome = filepath.Join(home, ".gemini")
	}
	p := &Provider{Name: ProviderAntigravity, Label: "Antigravity", ledger: db}
	p.scan = func(ctx context.Context) error { _, err := scanAntigravity(ctx, db, geminiHome); return err }
	p.load = func(ctx context.Context) ([]Entry, error) { return loadAntigravity(ctx, db) }
	return p, nil
}

// ---- discovery ----

// antigravityRoots are the folders of the Antigravity installs. Each keeps a
// "brain" folder of transcripts and a "conversations" folder of databases,
// both named by conversation ID.
func antigravityRoots(geminiHome string) []string {
	return []string{
		filepath.Join(geminiHome, "antigravity"),
		filepath.Join(geminiHome, "antigravity-ide"),
		filepath.Join(geminiHome, "antigravity-cli"),
	}
}

func pathWithin(file, root string) bool {
	relative, err := filepath.Rel(root, file)
	return err == nil && relative != "" && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// agSource is one conversation. Path names it in the ledger state: its
// transcript, or its database when it has no transcript (a conversation run
// by another agent, or one whose transcript is gone).
type agSource struct {
	Path       string
	Transcript string
	DB         string
}

// listAntigravitySources returns every conversation and every source file
// that exists. complete is false when a folder that held a tracked
// conversation could not be listed.
func listAntigravitySources(geminiHome string, known []string) ([]agSource, map[string]bool, bool) {
	var sources []agSource
	live := map[string]bool{}
	complete := true
	readDir := func(dir string) []fs.DirEntry {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A missing optional folder is authoritative only when it never
			// contained a tracked conversation.
			if !errors.Is(err, fs.ErrNotExist) {
				complete = false
			}
			for _, file := range known {
				if pathWithin(file, dir) {
					complete = false
					break
				}
			}
		}
		return entries
	}
	for _, root := range antigravityRoots(geminiHome) {
		conversations := filepath.Join(root, "conversations")
		withTranscript := map[string]bool{}
		var found []agSource
		for _, entry := range readDir(filepath.Join(root, "brain")) {
			if !entry.IsDir() {
				continue
			}
			transcript := filepath.Join(root, "brain", entry.Name(), ".system_generated", "logs", "transcript.jsonl")
			st, err := os.Stat(transcript)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					complete = false
				}
				continue
			}
			if st.Mode().IsRegular() {
				withTranscript[entry.Name()] = true
				live[transcript] = true
				found = append(found, agSource{Path: transcript, Transcript: transcript, DB: filepath.Join(conversations, entry.Name()+".db")})
			}
		}
		for _, entry := range readDir(conversations) {
			id, isDB := strings.CutSuffix(entry.Name(), ".db")
			if !isDB || id == "" || strings.HasPrefix(id, ".") || !entry.Type().IsRegular() {
				continue
			}
			db := filepath.Join(conversations, entry.Name())
			live[db] = true
			if !withTranscript[id] {
				found = append(found, agSource{Path: db, DB: db})
			}
		}
		sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
		sources = append(sources, found...)
	}
	return sources, live, complete
}

var agConversationID = regexp.MustCompile(`[/\\]brain[/\\]([^/\\]+)|[/\\]conversations[/\\]([^/\\]+)\.db$`)

// agSessionName is the conversation ID in a transcript or database path.
func agSessionName(file string) string {
	if m := agConversationID.FindStringSubmatch(file); m != nil {
		return m[1] + m[2]
	}
	return hashText(file)
}

// agFingerprint is what a conversation's records were last read from. A
// conversation whose files all still match is not read again.
type agFingerprint struct {
	Parser     int      `json:"parser"`
	Transcript [2]int64 `json:"transcript"`
	DB         [2]int64 `json:"db"`
	WAL        [2]int64 `json:"wal"`
}

// antigravityParserVersion must change whenever the same files would give
// different records, so every conversation is read again once.
const antigravityParserVersion = 3

// fingerprint stats the conversation's files; ok is false when one could not
// be checked.
func (s agSource) fingerprint() (agFingerprint, bool) {
	ok := true
	stat := func(path string) [2]int64 {
		if path == "" {
			return [2]int64{-1, 0}
		}
		st, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return [2]int64{-1, 0}
		}
		if err != nil {
			ok = false
			return [2]int64{}
		}
		return [2]int64{st.Size(), st.ModTime().UnixNano()}
	}
	fp := agFingerprint{Parser: antigravityParserVersion, Transcript: stat(s.Transcript), DB: stat(s.DB), WAL: stat(s.DB + "-wal")}
	return fp, ok
}

// ---- protobuf metadata ----

type protoField struct {
	num    uint64
	varint bool
	value  uint64
	safe   bool
	bytes  []byte
}

var errProto = errors.New("invalid Antigravity protobuf")

func protoVarint(buf []byte, offset int) (uint64, int, error) {
	var result uint64
	for count := 0; count < 10; count++ {
		if offset >= len(buf) {
			return 0, 0, errProto
		}
		b := buf[offset]
		offset++
		if count == 9 && b > 1 {
			return 0, 0, errProto
		}
		result |= uint64(b&0x7f) << (7 * count)
		if b&0x80 == 0 {
			return result, offset, nil
		}
	}
	return 0, 0, errProto
}

func protoFields(buf []byte) ([]protoField, error) {
	var fields []protoField
	offset := 0
	for offset < len(buf) {
		tag, next, err := protoVarint(buf, offset)
		if err != nil {
			return nil, err
		}
		if tag > 9007199254740991 {
			return nil, errProto
		}
		offset = next
		num, wire := tag/8, tag&7
		if num == 0 {
			return nil, errProto
		}
		switch wire {
		case 0:
			value, next, err := protoVarint(buf, offset)
			if err != nil {
				return nil, err
			}
			offset = next
			fields = append(fields, protoField{num: num, varint: true, value: value, safe: value <= 9007199254740991})
		case 2:
			length, next, err := protoVarint(buf, offset)
			if err != nil {
				return nil, err
			}
			offset = next
			if length > uint64(len(buf)-offset) {
				return nil, errProto
			}
			fields = append(fields, protoField{num: num, bytes: buf[offset : offset+int(length)]})
			offset += int(length)
		case 1:
			if len(buf)-offset < 8 {
				return nil, errProto
			}
			offset += 8
		case 5:
			if len(buf)-offset < 4 {
				return nil, errProto
			}
			offset += 4
		default:
			return nil, errProto
		}
	}
	return fields, nil
}

func firstField(fields []protoField, num uint64) (protoField, bool) {
	for _, f := range fields {
		if f.num == num {
			return f, true
		}
	}
	return protoField{}, false
}

// messageField returns a length-delimited field. A zero varint in its place
// counts as absent; any other varint makes the whole decode fail.
func messageField(fields []protoField, num uint64) ([]byte, bool, error) {
	f, ok := firstField(fields, num)
	if !ok {
		return nil, false, nil
	}
	if f.varint {
		if f.value == 0 {
			return nil, false, nil
		}
		return nil, false, errProto
	}
	return f.bytes, true, nil
}

func countField(fields []protoField, num uint64) (int64, bool) {
	f, ok := firstField(fields, num)
	if !ok || !f.varint || !f.safe {
		return 0, false
	}
	return int64(f.value), true
}

type agUsage struct {
	ModelCode     *int64
	Uncached      int64
	Cached        int64
	CacheCreation int64
	OutputTotal   int64
	TextOutput    int64
	Reasoning     int64
	OutputSplit   string
	ResponseID    *string
}

// ModelUsageStats field numbers as written by Antigravity:
// 1=model enum (NOT tokens), 2=input, 3=total output, 4=cache write,
// 5=cache read, 9=thinking output, 10=response output, 11=response ID.
func agUsageStats(buf []byte) (*agUsage, error) {
	fields, err := protoFields(buf)
	if err != nil {
		return nil, err
	}
	any := false
	for _, n := range []uint64{2, 3, 4, 5, 9, 10} {
		if _, ok := countField(fields, n); ok {
			any = true
		}
	}
	if !any {
		return nil, nil
	}
	total, hasTotal := countField(fields, 3)
	reasoning, hasReasoning := countField(fields, 9)
	text, hasText := countField(fields, 10)
	if !hasTotal {
		total = reasoning + text
	}
	if !hasReasoning && hasText && total >= text {
		reasoning, hasReasoning = total-text, true
	}
	if !hasText && hasReasoning && total >= reasoning {
		text, hasText = total-reasoning, true
	}
	split := "split"
	if !hasReasoning || !hasText || reasoning+text != total {
		reasoning, text, split = 0, total, "combined"
	}
	out := &agUsage{OutputTotal: total, TextOutput: text, Reasoning: reasoning, OutputSplit: split}
	if code, ok := countField(fields, 1); ok {
		out.ModelCode = &code
	}
	out.Uncached, _ = countField(fields, 2)
	out.Cached, _ = countField(fields, 5)
	out.CacheCreation, _ = countField(fields, 4)
	if f, ok := firstField(fields, 11); ok && !f.varint {
		id := string(f.bytes)
		out.ResponseID = &id
	}
	return out, nil
}

type agTurn struct {
	Model         string
	HasModel      bool
	SelectedLabel string
	ContextTokens int64
	Usage         *agUsage
	UsageSource   string
}

func agGenInfo(buf []byte) (turn agTurn, lastStep int64, hasStep bool, ok bool) {
	root, err := protoFields(buf)
	if err != nil {
		return
	}
	f1, present, err := messageField(root, 1)
	if err != nil || !present {
		return
	}
	inner, err := protoFields(f1)
	if err != nil {
		return
	}
	if value, present, err := messageField(inner, 19); err != nil {
		return
	} else if present {
		turn.Model, turn.HasModel = jsTrim(string(value)), true
	}
	if value, present, err := messageField(inner, 21); err != nil {
		return
	} else if present {
		turn.SelectedLabel = jsTrim(string(value))
	}
	if f9, present, err := messageField(inner, 9); err != nil {
		return
	} else if present {
		fields9, err := protoFields(f9)
		if err != nil {
			return
		}
		if f10, present, err := messageField(fields9, 10); err != nil {
			return
		} else if present {
			fields10, err := protoFields(f10)
			if err != nil {
				return
			}
			if tok, ok := countField(fields10, 1); ok {
				turn.ContextTokens = tok
			}
		}
	}
	for _, f := range inner {
		if f.num != 20 || (f.varint && f.value == 0) {
			continue
		}
		if f.varint {
			return
		}
		kv, err := protoFields(f.bytes)
		if err != nil {
			return
		}
		k, hasK, errK := messageField(kv, 1)
		v, hasV, errV := messageField(kv, 2)
		if errK != nil || (hasK && string(k) == "last_step_index" && errV != nil) {
			return
		}
		if hasK && string(k) == "last_step_index" && hasV {
			raw := jsTrim(string(v))
			if raw != "" && strings.Trim(raw, "0123456789") == "" {
				if parsed, err := strconv.ParseFloat(raw, 64); err == nil {
					if n, safe := safeIntFloat(parsed); safe {
						lastStep, hasStep = n, true
					}
				}
			}
		}
	}
	if f4, present, err := messageField(inner, 4); err != nil {
		return
	} else if present {
		usage, err := agUsageStats(f4)
		if err != nil {
			return
		}
		turn.Usage = usage
	}
	return turn, lastStep, hasStep, true
}

// agConversation is what a conversation database says about its turns.
type agConversation struct {
	// Turns holds the usage metadata by step. It is nil when the database is
	// missing or unreadable, so transcript counters are used instead; an empty
	// map means it was read fine but holds no usage metadata yet.
	Turns map[int64]agTurn
	// Planner lists the planner response steps in order, and Times the time
	// each one recorded.
	Planner []int64
	Times   map[int64]time.Time
}

// agStepTime reads a step's google.protobuf.Timestamp (field 1 of its
// metadata).
func agStepTime(fields []protoField) (time.Time, bool) {
	value, present, err := messageField(fields, 1)
	if err != nil || !present {
		return time.Time{}, false
	}
	stamp, err := protoFields(value)
	if err != nil {
		return time.Time{}, false
	}
	seconds, ok := countField(stamp, 1)
	if !ok || seconds <= 0 {
		return time.Time{}, false
	}
	nanos, _ := countField(stamp, 2)
	if nanos >= int64(time.Second) {
		return time.Time{}, false
	}
	return time.Unix(seconds, nanos).UTC(), true
}

// readConversationDB reads a conversation database. complete is false when
// the database exists but could not be read in full, so its records may be
// worse than a later read would give.
func readConversationDB(ctx context.Context, path string) (conv agConversation, complete bool) {
	if path == "" {
		return conv, true
	}
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return conv, errors.Is(err, fs.ErrNotExist)
	}
	db, err := sql.Open("sqlite", sqliteURI(path, "mode=ro&_pragma=busy_timeout(15000)&_pragma=query_only(1)"))
	if err != nil {
		return conv, false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `SELECT idx, data FROM gen_metadata ORDER BY idx`)
	if err != nil {
		return conv, false
	}
	steps := map[int64]agTurn{}
	for rows.Next() {
		var idx any
		var data any
		if err := rows.Scan(&idx, &data); err != nil {
			rows.Close()
			return conv, false
		}
		blob, ok := data.([]byte)
		if !ok {
			continue
		}
		turn, last, hasStep, ok := agGenInfo(blob)
		if ok && hasStep && (turn.Usage != nil || turn.ContextTokens > 0) {
			turn.UsageSource = "gen-metadata"
			steps[last+1] = turn
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return conv, false
	}
	rows.Close()
	conv.Turns = steps
	conv.Times = map[int64]time.Time{}
	stepRows, err := db.QueryContext(ctx, `SELECT idx, metadata FROM steps WHERE step_type=15 ORDER BY idx`)
	if err != nil {
		return conv, false
	}
	defer stepRows.Close()
	for stepRows.Next() {
		var idx int64
		var data any
		if err := stepRows.Scan(&idx, &data); err != nil {
			continue
		}
		conv.Planner = append(conv.Planner, idx)
		blob, ok := data.([]byte)
		if !ok {
			continue
		}
		fields, err := protoFields(blob)
		if err != nil {
			continue
		}
		if at, ok := agStepTime(fields); ok {
			conv.Times[idx] = at
		}
		if _, exists := steps[idx]; exists {
			continue
		}
		f9, present, err := messageField(fields, 9)
		if err != nil || !present {
			continue
		}
		usage, err := agUsageStats(f9)
		if err != nil || usage == nil {
			continue
		}
		steps[idx] = agTurn{Usage: usage, UsageSource: "steps-metadata"}
	}
	return conv, stepRows.Err() == nil
}

// ---- transcript ----

var (
	agSelectionChange = regexp.MustCompile("(?i)changed setting `Model Selection` from .*? to ([^`\\n]+?)\\.(?:\\s+|$)")
	agModelSelection  = regexp.MustCompile("(?i)changed setting `Model Selection` from .*? to ([^`\\n]+?)(?:\\s*\\([^)]*\\))?\\.(?:\\s+|$)")
	agParenthetical   = regexp.MustCompile(`\([^)]*\)`)
	agEffortWords     = regexp.MustCompile(`(?i)\b(thinking|xhigh|high|medium|low|fast)\b`)
	agNonSlug         = regexp.MustCompile(`[^a-z0-9.]+`)
	agEdgeDashes      = regexp.MustCompile(`^-+|-+$`)
	agDashRuns        = regexp.MustCompile(`-{2,}`)
	agFlashTier       = regexp.MustCompile(`^gemini-3\.8-flash-(?:tiered|n)$`)
	// A step that names no choice reports the model ID it was served by, which
	// may carry a serving variant of the chosen model ("gemini-4-argon-s").
	agServingVariant = regexp.MustCompile(`^(gemini-\d+(?:\.\d+)?-[a-z]+)-(?:s|trial)$`)
	agKnownPrefix    = regexp.MustCompile(`^(gemini|claude|gpt)-`)
	agProRouteLabel  = regexp.MustCompile(`(?i)^Gemini 3\.1 Pro(?: \((?:High|Medium|Low)\))?$`)
)

func normalizeAGModel(name string) string {
	slug := jsTrim(name)
	if slug == "" {
		return ""
	}
	slug = agParenthetical.ReplaceAllString(slug, " ")
	slug = agEffortWords.ReplaceAllString(slug, " ")
	slug = agNonSlug.ReplaceAllString(strings.ToLower(slug), "-")
	slug = agEdgeDashes.ReplaceAllString(slug, "")
	slug = agDashRuns.ReplaceAllString(slug, "-")
	if slug == "" {
		return ""
	}
	if agFlashTier.MatchString(slug) {
		return "gemini-3.8-flash"
	}
	if m := agServingVariant.FindStringSubmatch(slug); m != nil {
		return m[1]
	}
	for _, marker := range []string{"gemini", "claude", "gpt"} {
		if index := strings.Index(slug, marker); index >= 0 {
			slug = slug[index:]
			break
		}
	}
	if agKnownPrefix.MatchString(slug) {
		return slug
	}
	return "antigravity-" + slug
}

// Orchestrator is a picker option, not a model: a planner (Carbon, Argon or Neon,
// named in the option) plans and reviews, and Gemini 4 Hydrogen implements.
// Its steps report the role in their model id, so each step is priced as the
// model that did it.
var agOrchestratorOption = regexp.MustCompile(`(?i)^\s*orchestrator\s*\(\s*(carbon|argon|neon)\s*\)\s*$`)

// agOrchestratorPlanner returns the planner model of an Orchestrator option.
func agOrchestratorPlanner(label string) string {
	if m := agOrchestratorOption.FindStringSubmatch(label); m != nil {
		return "gemini-4-" + strings.ToLower(m[1])
	}
	return ""
}

// agStepModel returns the model of one step while Orchestrator is selected.
func agStepModel(model, planner, reported string) string {
	if planner == "" {
		return model
	}
	switch {
	case agWorkerStep(reported):
		return "gemini-4-hydrogen"
	case strings.Contains(strings.ToLower(reported), "orchestrator"), model == "antigravity-orchestrator":
		return planner
	}
	return model
}

// agNextPlanner tracks whether Orchestrator is selected. Its implementation
// steps carry their own label ("Gemini Flash"); only a choice of another
// model ends it.
func agNextPlanner(planner, selected, reported string) string {
	if selected == "" || agWorkerStep(reported) {
		return planner
	}
	return agOrchestratorPlanner(selected)
}

func agWorkerStep(reported string) bool {
	return strings.Contains(strings.ToLower(reported), "worker")
}

func isCJK(r rune) bool {
	return (r >= 0x3400 && r <= 0x4dbf) || (r >= 0x4e00 && r <= 0x9fff) || (r >= 0x3040 && r <= 0x30ff)
}

func estimateTokens(text string) int64 {
	var cjk, other int64
	for _, r := range text {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func valueTokens(value jsValue, ok bool) int64 {
	if !ok || value.kind == 'n' {
		return 0
	}
	if value.kind == 's' {
		return estimateTokens(value.s)
	}
	return estimateTokens(value.stringify())
}

func contextTokensOf(event jsValue) int64 {
	tokens := valueTokens(event.get("content"))
	if kind, _ := event.str("type"); kind == "PLANNER_RESPONSE" {
		if calls, ok := event.get("tool_calls"); ok && calls.truthy() {
			tokens += valueTokens(calls, true)
		}
	}
	return tokens
}

func selectionTimeline(events []jsValue, parsed []bool) []string {
	selected, pending, active := "", "", false
	out := make([]string, len(events))
	for i, event := range events {
		if !parsed[i] {
			out[i] = selected
			continue
		}
		kind, _ := event.str("type")
		change := ""
		if kind == "USER_INPUT" || kind == "USER_SETTINGS_CHANGE" {
			if content, ok := event.str("content"); ok {
				if m := agSelectionChange.FindStringSubmatch(content); m != nil {
					change = jsTrim(m[1])
				}
			}
		}
		switch {
		case kind == "USER_INPUT":
			if change != "" {
				selected = change
			} else if pending != "" {
				selected = pending
			}
			pending, active = "", true
		case change != "":
			if active {
				pending = change
			} else {
				selected = change
			}
		case kind == "TASK_COMPLETED" || kind == "TURN_END" || kind == "CANCELLED":
			active = false
		}
		out[i] = selected
	}
	return out
}

func strPtr(value string) *string { return &value }

// agExactTotals is the usage of one planner response with recorded counts.
func agExactTotals(u *agUsage) agTotals {
	t := agTotals{Input: u.Uncached, Cached: u.Cached, CacheCreation: u.CacheCreation, Output: u.TextOutput, Reasoning: u.Reasoning}
	t.Total = t.Input + t.Cached + t.Output + t.Reasoning + t.CacheCreation
	t.BillableTotal, t.ConversationCount = t.Total, 1
	return t
}

// setTurn copies what the conversation database reported for the step.
func (r *AntigravityRecord) setTurn(turn *agTurn) {
	if turn.Usage != nil && turn.Usage.ResponseID != nil && *turn.Usage.ResponseID != "" {
		r.ResponseID = turn.Usage.ResponseID
	}
	if turn.Model != "" {
		r.ReportedModel = strPtr(turn.Model)
	}
	if turn.Usage != nil {
		r.ModelCode = turn.Usage.ModelCode
	}
}

// parseAntigravityConversation reads one conversation: its transcript when it
// has one, and its database for every planner response the transcript does
// not hold. Those are a conversation run by another agent, which has no
// transcript, and the start of a long conversation, which Antigravity trims
// from the transcript. readFully is false when a file could not be read in
// full, so a later read may give better records.
func parseAntigravityConversation(ctx context.Context, source agSource) (records []AntigravityRecord, readFully bool) {
	conv, readFully := readConversationDB(ctx, source.DB)
	fromDB := agDatabaseRecords(conv, hashText(agSessionName(source.Path)))
	if source.Transcript == "" {
		return fromDB, readFully
	}
	data, err := os.ReadFile(source.Transcript)
	if err != nil {
		return nil, false
	}
	records = parseAntigravityTranscript(source.Transcript, conv, data)
	covered := make(map[string]bool, len(records))
	for _, record := range records {
		covered[record.key()] = true
	}
	for _, record := range fromDB {
		if !covered[record.key()] {
			records = append(records, record)
		}
	}
	return records, readFully
}

// agDatabaseRecords reads a conversation from its database alone, emitting one
// record per planner response with recorded usage. The model is followed as
// the transcript would follow it: the choice each step names, and the
// Orchestrator roles.
func agDatabaseRecords(conv agConversation, session string) []AntigravityRecord {
	var out []AntigravityRecord
	currentModel, planner := "", ""
	for _, idx := range conv.Planner {
		turn, found := conv.Turns[idx]
		if !found {
			continue
		}
		planner = agNextPlanner(planner, turn.SelectedLabel, turn.Model)
		name := turn.SelectedLabel
		if name == "" {
			name = turn.Model
		}
		if normalized := normalizeAGModel(name); normalized != "" {
			currentModel = normalized
		}
		at, hasTime := conv.Times[idx]
		if turn.Usage == nil || !hasTime {
			continue
		}
		totals := agExactTotals(turn.Usage)
		if totals.Total == 0 {
			continue
		}
		modelName := agStepModel(currentModel, planner, turn.Model)
		if modelName == "" {
			modelName = "antigravity-unknown"
		}
		step := idx
		record := AntigravityRecord{
			Line: idx + 1, StepIndex: &step, Timestamp: jsISOTime(at), Model: modelName,
			Precision: "recorded", TokenSource: turn.UsageSource, OutputSplit: turn.Usage.OutputSplit,
			Totals: totals, Session: session, AccountingRevision: antigravityAccountingRevision, Source: agSourceDatabase,
		}
		if turn.SelectedLabel != "" {
			record.SelectedModel = strPtr(turn.SelectedLabel)
		}
		record.setTurn(&turn)
		out = append(out, record)
	}
	return out
}

// parseAntigravityTranscript walks one transcript from the start, emitting one
// record per billed planner response.
func parseAntigravityTranscript(file string, conv agConversation, data []byte) []AntigravityRecord {
	raw := string(data)
	if jsTrim(raw) == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = jsTrim(strings.TrimSuffix(line, "\r"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	events := make([]jsValue, len(lines))
	parsed := make([]bool, len(lines))
	for i, line := range lines {
		if value, err := parseJS([]byte(line)); err == nil {
			events[i], parsed[i] = value, true
		}
	}
	steps := conv.Turns
	selections := selectionTimeline(events, parsed)
	session := hashText(agSessionName(file))
	// The line and time of the latest planner response for each step index.
	// Only that response may take the step's database record.
	type stepUse struct {
		line int
		at   string
	}
	latest := map[int64]stepUse{}
	plannerStep := func(event jsValue) (int64, bool) {
		if kind, _ := event.str("type"); kind != "PLANNER_RESPONSE" {
			return 0, false
		}
		value, ok := event.get("step_index")
		if !ok || value.kind != 'f' || math.IsInf(value.f, 0) || math.IsNaN(value.f) {
			return 0, false
		}
		return safeIntFloat(value.f)
	}
	for i := range lines {
		if !parsed[i] {
			break
		}
		if n, ok := plannerStep(events[i]); ok {
			at, _ := events[i].str("created_at")
			latest[n] = stepUse{i, at}
		}
	}

	var out []AntigravityRecord
	currentModel, planner := "", ""
	var contextTokens, previousContext int64
	lastPlannerModel, hasLastPlanner := "", false
	for i := range lines {
		if !parsed[i] {
			break
		}
		event := events[i]
		kind, _ := event.str("type")
		if kind == "USER_INPUT" || kind == "USER_SETTINGS_CHANGE" {
			content, _ := event.str("content")
			if m := agModelSelection.FindStringSubmatch(content); m != nil {
				if normalized := normalizeAGModel(m[1]); normalized != "" {
					currentModel = normalized
				}
			}
		}
		eventContext := contextTokensOf(event)
		var turn *agTurn
		stepValue, hasStepValue := event.get("step_index")
		superseded := ""
		if n, ok := plannerStep(event); ok {
			if use := latest[n]; use.line != i {
				// An earlier use of a reused step index; the database holds
				// the latest use, not this one.
				at, _ := event.str("created_at")
				if at == use.at {
					// The same response written twice.
					contextTokens += eventContext
					continue
				}
				superseded = at
			} else if found, ok := steps[n]; ok {
				turn = &found
			}
		}
		var dbContext int64
		if turn != nil && turn.ContextTokens > 0 {
			dbContext = turn.ContextTokens
		}
		selectedName := selections[i]
		if turn != nil && turn.SelectedLabel != "" {
			selectedName = turn.SelectedLabel
		}
		reportedModel := ""
		if turn != nil {
			reportedModel = turn.Model
		}
		planner = agNextPlanner(planner, selectedName, reportedModel)
		if selectedName != "" || (turn != nil && turn.Model != "") {
			name := selectedName
			if name == "" {
				name = turn.Model
			}
			if normalized := normalizeAGModel(name); normalized != "" {
				currentModel = normalized
			}
		}
		timestampValue, hasTimestamp := event.get("created_at")
		if !hasTimestamp || !timestampValue.truthy() {
			contextTokens += eventContext
			continue
		}
		timestamp := jsString(timestampValue, true)
		if _, ok := parseJSTime(timestamp); !ok || timestampValue.kind != 's' {
			contextTokens += eventContext
			continue
		}
		modelName := currentModel
		if turn != nil {
			modelName = agStepModel(modelName, planner, turn.Model)
		} else {
			modelName = agStepModel(modelName, planner, "")
		}
		if modelName == "" {
			modelName = "antigravity-unknown"
		}
		var exact *agUsage
		exactFromDB := false
		if turn != nil && turn.Usage != nil {
			exact, exactFromDB = turn.Usage, true
		} else {
			exact = transcriptUsage(event)
		}
		var delta agTotals
		billed := false
		if kind == "PLANNER_RESPONSE" {
			if exact != nil {
				delta = agExactTotals(exact)
				billed = delta.Total > 0
				if dbContext > 0 {
					contextTokens = dbContext
				} else {
					contextTokens = exact.Uncached + exact.Cached
				}
			} else {
				if dbContext > 0 {
					contextTokens = dbContext
				}
				if hasLastPlanner && modelName != lastPlannerModel {
					previousContext = 0
				}
				inputDelta := contextTokens - previousContext
				if inputDelta < 0 {
					inputDelta = 0
				}
				// Only string content counts as planner output; tool calls are re-encoded.
				content, _ := event.str("content")
				outputTokens := estimateTokens(content) + valueTokens(event.get("tool_calls"))
				thinking, _ := event.str("thinking")
				reasoning := estimateTokens(thinking)
				delta.Input, delta.Output, delta.Reasoning = inputDelta, outputTokens, reasoning
				delta.Total = inputDelta + outputTokens + reasoning
				delta.BillableTotal, delta.ConversationCount = delta.Total, 1
				billed = delta.Total > 0
			}
		}
		if !billed {
			contextTokens += eventContext
			continue
		}
		record := AntigravityRecord{
			Line: int64(i + 1), Timestamp: timestamp, Model: modelName,
			Precision: "estimated", TokenSource: "text-estimate", OutputSplit: "estimated",
			Totals: delta, Session: session, AccountingRevision: antigravityAccountingRevision, Superseded: superseded,
		}
		if hasStepValue && stepValue.kind == 'f' {
			if n, ok := safeIntFloat(stepValue.f); ok {
				record.StepIndex = &n
			}
		}
		if selectedName != "" {
			record.SelectedModel = strPtr(selectedName)
		}
		if turn != nil {
			record.setTurn(turn)
		}
		if exact != nil {
			record.Precision, record.OutputSplit = "recorded", exact.OutputSplit
			if exactFromDB {
				record.TokenSource = turn.UsageSource
			} else {
				record.TokenSource = "transcript-counters"
			}
		}
		out = append(out, record)
		previousContext = contextTokens
		lastPlannerModel, hasLastPlanner = modelName, true
		contextTokens += eventContext
	}
	return out
}

func transcriptUsage(event jsValue) *agUsage {
	input, ok1 := event.safeInt("input_tokens")
	cached, ok2 := event.safeInt("cache_read_tokens")
	output, ok3 := event.safeInt("output_tokens")
	if !ok1 || !ok2 || !ok3 || input < 0 || cached < 0 || output < 0 || input+cached+output == 0 {
		return nil
	}
	write := int64(0)
	if value, ok := event.get("cache_write_tokens"); ok && value.kind != 'n' {
		n, safe := value.safeIntSelf()
		if !safe || n < 0 {
			return nil
		}
		write = n
	}
	return &agUsage{Uncached: input, Cached: cached, CacheCreation: write, OutputTotal: output, TextOutput: output, OutputSplit: "combined"}
}

func (v jsValue) safeIntSelf() (int64, bool) {
	if v.kind != 'f' {
		return 0, false
	}
	return safeIntFloat(v.f)
}

// ---- scan ----

type agScan struct {
	FilesProcessed        int      `json:"filesProcessed"`
	UnchangedSources      int      `json:"unchangedSources"`
	Observations          int      `json:"observations"`
	CorrectedRecords      int      `json:"correctedRecords"`
	AccountingRevision    int      `json:"accountingRevision"`
	SourceFiles           int      `json:"sourceFiles"`
	InventoryComplete     bool     `json:"inventoryComplete"`
	MissingSources        int      `json:"missingSources"`
	MissingSourceSessions []string `json:"missingSourceSessions"`
	LastScan              string   `json:"lastScan"`
	NextScan              string   `json:"nextScan"`
}

func scanAntigravity(ctx context.Context, db *ledgerDB, geminiHome string) (agScan, error) {
	var known []string
	if found, err := db.state(ctx, "known-sources", &known); err != nil {
		return agScan{}, err
	} else if !found {
		// Older ledgers kept per-file cursors; their keys are the known sources.
		var cursors struct {
			Files map[string]json.RawMessage `json:"files"`
		}
		if _, err := db.state(ctx, "cursors", &cursors); err != nil {
			return agScan{}, err
		}
		for file := range cursors.Files {
			known = append(known, file)
		}
		sort.Strings(known)
	}
	sources, live, complete := listAntigravitySources(geminiHome, known)
	var fingerprints map[string]agFingerprint
	if _, err := db.state(ctx, "source-fingerprints", &fingerprints); err != nil {
		return agScan{}, err
	}
	nextFingerprints := map[string]agFingerprint{}
	var missingSessions []string
	missing := 0
	for _, file := range known {
		if !live[file] {
			missing++
			missingSessions = append(missingSessions, hashText(agSessionName(file)))
		}
	}
	missingSessions = uniqueStrings(missingSessions)

	rows, err := db.records(ctx)
	if err != nil {
		return agScan{}, err
	}
	existing := make(map[string]rawRecord, len(rows))
	for _, row := range rows {
		existing[row.ID] = row
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return agScan{}, err
	}
	defer tx.Rollback()
	now := time.Now()
	stamp := jsISOTime(now)
	observations, corrected, processed, unchanged := 0, 0, 0, 0
	for _, source := range sources {
		// Files are checked before they are read, so a change made while
		// reading is seen by the next scan.
		fingerprint, checked := source.fingerprint()
		if checked && fingerprints[source.Path] == fingerprint {
			nextFingerprints[source.Path] = fingerprint
			unchanged++
			continue
		}
		records, readFully := parseAntigravityConversation(ctx, source)
		processed++
		if checked && readFully {
			nextFingerprints[source.Path] = fingerprint
		}
		for _, record := range records {
			id := record.key()
			next, err := marshalJS(record)
			if err != nil {
				return agScan{}, err
			}
			if old, ok := existing[id]; ok {
				var prior AntigravityRecord
				if err := jsonUnmarshal(old.Payload, &prior); err != nil {
					return agScan{}, err
				}
				// Never replace a richer exact split with transcript-only or estimated data.
				if prior.Precision == "recorded" && record.Precision != "recorded" {
					continue
				}
				// A step read from its transcript keeps the model the transcript
				// showed when the transcript is later gone.
				if record.Source == agSourceDatabase && prior.Source != agSourceDatabase {
					continue
				}
				if prior.AccountingRevision >= 3 && (prior.TokenSource == "gen-metadata" || prior.TokenSource == "steps-metadata") && record.TokenSource == "transcript-counters" {
					continue
				}
				record.RecoveryEvidence = prior.RecoveryEvidence
				if record.RecoveryEvidence != nil {
					if next, err = marshalJS(record); err != nil {
						return agScan{}, err
					}
				}
				if prior.Totals != record.Totals || prior.Precision != record.Precision {
					if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO record_revisions VALUES(?,?,?,?,?,?)`,
						id, hashText(old.Payload), old.Payload, next, "native-usage-schema-v3", stamp); err != nil {
						return agScan{}, err
					}
					corrected++
				} else if old.Payload == next {
					observations++
					continue
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO records(id,payload) VALUES(?,?)
				ON CONFLICT(id) DO UPDATE SET payload=excluded.payload
				WHERE NOT(json_extract(records.payload,'$.precision')='recorded' AND json_extract(excluded.payload,'$.precision')='estimated')`, id, next); err != nil {
				return agScan{}, err
			}
			existing[id] = rawRecord{ID: id, Payload: next}
			observations++
		}
	}
	if missingSessions == nil {
		missingSessions = []string{}
	}
	scan := agScan{
		FilesProcessed: processed, UnchangedSources: unchanged, Observations: observations, CorrectedRecords: corrected,
		AccountingRevision: antigravityAccountingRevision, SourceFiles: len(sources), InventoryComplete: complete,
		MissingSources: missing, MissingSourceSessions: missingSessions,
		LastScan: stamp, NextScan: jsISOTime(now.Add(time.Hour)),
	}
	for _, source := range sources {
		known = append(known, source.Path)
	}
	if err := putState(ctx, tx, "known-sources", uniqueStrings(known)); err != nil {
		return agScan{}, err
	}
	if err := putState(ctx, tx, "source-fingerprints", nextFingerprints); err != nil {
		return agScan{}, err
	}
	if err := putState(ctx, tx, "scan", scan); err != nil {
		return agScan{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO state(id,payload) VALUES('accounting-revision','3')`); err != nil {
		return agScan{}, err
	}
	return scan, tx.Commit()
}

// ---- pricing ----

type agRate struct {
	From                                 time.Time
	Input, Output, CacheRead, CacheWrite float64
}

// Public Standard API equivalent, never subscription charges.
// Verified 2026-11-19: https://aistudio.google.com/docs/pricing
var agRateCards = map[string][]agRate{
	"gemini-4-carbon":   {{mustDate("2026-11-10T00:00:00Z"), 2, 10, .1, 0}, {mustDate("2026-12-01T00:00:00Z"), 4, 20, .2, 0}},
	"gemini-4-hydrogen": {{mustDate("2026-11-12T00:00:00Z"), 1.5, 7.5, .075, 0}},
	"gemini-4-neon":     {{mustDate("2026-11-18T00:00:00Z"), 3, 15, .15, 0}},
	"claude-opus-4-6":   {{mustDate("2026-02-05T00:00:00Z"), 5, 25, .5, 6.25}},
	"claude-opus-5-5":   {{mustDate("2026-09-22T00:00:00Z"), 4, 20, .2, 5}},
	"claude-sonnet-5-5": {{mustDate("2026-09-28T00:00:00Z"), 2, 10, .1, 2.5}},
	"claude-sonnet-4-6": {{claudeLaunchRate, 3, 15, .3, 3.75}},
	"gemini-3.1-pro":    {{mustDate("2026-02-19T00:00:00Z"), 2, 12, .2, 0}},
	"gemini-4-argon":    {{mustDate("2026-10-13T00:00:00Z"), 2, 10, .1, 0}, {mustDate("2026-12-01T00:00:00Z"), 4, 20, .2, 0}},
	"gemini-3.8-flash":  {{mustDate("2026-09-02T00:00:00Z"), .75, 3.75, .075, 0}, {mustDate("2027-01-01T00:00:00Z"), 1.5, 7.5, .15, 0}},
}

func agHistoricalRate(modelName string, at time.Time) *agRate {
	cards := agRateCards[canonicalModel(modelName)]
	var result *agRate
	for index := range cards {
		if !at.Before(cards[index].From) {
			card := cards[index]
			result = &card
		}
	}
	return result
}

func priceAntigravity(r AntigravityRecord, at time.Time, overrides []DatedOverride) Price {
	// The routing ID alone is ambiguous; price only the records with an
	// explicit Gemini 3.1 Pro choice, never guess from the ID.
	if r.ReportedModel != nil && *r.ReportedModel == "gemini-pro-default" &&
		!(r.Model == "gemini-3.1-pro" && r.SelectedModel != nil && agProRouteLabel.MatchString(*r.SelectedModel)) {
		return Price{ReasonKind: "ambiguous_routing_model", Reason: "路由模型缺少可確認的歷史選擇"}
	}
	t := r.Totals
	var rate *agRate
	manual := pickOverride(overrides, r.Model, taipeiDay(at), t.Input+t.Cached, canonicalModel)
	if manual != nil {
		rate = &agRate{Input: manual.Input, Output: manual.Output, CacheRead: manual.CacheRead, CacheWrite: manual.CacheWrite}
	} else {
		rate = agHistoricalRate(r.Model, at)
	}
	if rate == nil {
		return unpriced("unverified_rate", "模型或使用日期尚無已核實費率")
	}
	if manual == nil && r.Model == "gemini-3.1-pro" && t.Input+t.Cached > 200000 {
		rate = &agRate{Input: 4, Output: 18, CacheRead: .4}
	}
	if manual == nil && t.CacheCreation > 0 && rate.CacheWrite == 0 {
		return unpriced("cache_write_rate_missing", "快取寫入費率未確認")
	}
	return Price{
		Priced: true, RegularNano: usdNano(t.Input, rate.Input), CachedNano: usdNano(t.Cached, rate.CacheRead),
		CacheWriteNano: usdNano(t.CacheCreation, rate.CacheWrite), OutputNano: usdNano(t.Output+t.Reasoning, rate.Output),
	}
}

func loadAntigravity(ctx context.Context, db *ledgerDB) ([]Entry, error) {
	rows, err := db.records(ctx)
	if err != nil {
		return nil, err
	}
	overrides := db.overrides(ctx)
	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		var r AntigravityRecord
		if err := jsonUnmarshal(row.Payload, &r); err != nil {
			return nil, err
		}
		at, ok := parseJSTime(r.Timestamp)
		if !ok {
			continue
		}
		t := r.Totals
		out = append(out, Entry{
			ID: "antigravity:" + row.ID, Session: r.Session, Timestamp: at, Model: canonicalModel(r.Model), Mode: model.ModeStandard,
			Precision: r.Precision, TokenSource: r.TokenSource,
			Usage: model.TokenUsage{
				Input: t.Input + t.Cached + t.CacheCreation, CachedInput: t.Cached, CacheWriteInput: t.CacheCreation,
				Output: t.Output + t.Reasoning, ReasoningOutput: t.Reasoning, Total: t.Total,
			},
			Price: priceAntigravity(r, at, overrides),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}
