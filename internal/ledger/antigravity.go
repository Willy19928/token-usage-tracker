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
}

func (r AntigravityRecord) key() string {
	if r.StepIndex != nil {
		return "step-v2:" + r.Session + ":" + strconv.FormatInt(*r.StepIndex, 10)
	}
	return "step-v2:" + r.Session + ":" + strconv.FormatInt(r.Line, 10)
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

func antigravityBrainDirs(geminiHome string) []string {
	return []string{
		filepath.Join(geminiHome, "antigravity", "brain"),
		filepath.Join(geminiHome, "antigravity-ide", "brain"),
		filepath.Join(geminiHome, "antigravity-cli", "brain"),
	}
}

func pathWithin(file, root string) bool {
	relative, err := filepath.Rel(root, file)
	return err == nil && relative != "" && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func listAntigravityTranscripts(geminiHome string, known []string) ([]string, bool) {
	var files []string
	complete := true
	for _, dir := range antigravityBrainDirs(geminiHome) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// A missing optional install directory is authoritative only when it
			// never contained a tracked session.
			if !errors.Is(err, fs.ErrNotExist) {
				complete = false
			}
			for _, file := range known {
				if pathWithin(file, dir) {
					complete = false
					break
				}
			}
			continue
		}
		var found []string
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			transcript := filepath.Join(dir, entry.Name(), ".system_generated", "logs", "transcript.jsonl")
			st, err := os.Stat(transcript)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					complete = false
				}
				continue
			}
			if st.Mode().IsRegular() {
				found = append(found, transcript)
			}
		}
		sort.Strings(found)
		files = append(files, found...)
	}
	return files, complete
}

var (
	agBrainSession = regexp.MustCompile(`[/\\]brain[/\\]([^/\\]+)`)
	agDBPath       = regexp.MustCompile(`^(.*)[/\\]brain[/\\]([^/\\]+)[/\\]\.system_generated[/\\]logs[/\\]transcript.*\.jsonl$`)
)

func agSessionName(file string) string {
	if m := agBrainSession.FindStringSubmatch(file); m != nil {
		return m[1]
	}
	return hashText(file)
}

func agConversationDB(transcript string) string {
	m := agDBPath.FindStringSubmatch(transcript)
	if m == nil {
		return ""
	}
	return filepath.Join(m[1], "conversations", m[2]+".db")
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

// readConversationDB returns nil when the database is missing or unreadable, so
// transcript counters are used instead; an empty map means it was read fine
// but holds no usage metadata yet.
func readConversationDB(ctx context.Context, path string) map[int64]agTurn {
	if path == "" {
		return nil
	}
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	db, err := sql.Open("sqlite", sqliteURI(path, "mode=ro&_pragma=busy_timeout(15000)&_pragma=query_only(1)"))
	if err != nil {
		return nil
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `SELECT idx, data FROM gen_metadata ORDER BY idx`)
	if err != nil {
		return nil
	}
	steps := map[int64]agTurn{}
	for rows.Next() {
		var idx any
		var data any
		if err := rows.Scan(&idx, &data); err != nil {
			rows.Close()
			return nil
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
		return nil
	}
	rows.Close()
	stepRows, err := db.QueryContext(ctx, `SELECT idx, metadata FROM steps WHERE step_type=15`)
	if err != nil {
		return steps
	}
	defer stepRows.Close()
	for stepRows.Next() {
		var idx int64
		var data any
		if err := stepRows.Scan(&idx, &data); err != nil {
			continue
		}
		blob, ok := data.([]byte)
		if _, exists := steps[idx]; exists || !ok {
			continue
		}
		fields, err := protoFields(blob)
		if err != nil {
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
	return steps
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
	agKnownPrefix     = regexp.MustCompile(`^(gemini|claude|gpt)-`)
	agProRouteLabel   = regexp.MustCompile(`(?i)^Gemini 3\.1 Pro(?: \((?:High|Medium|Low)\))?$`)
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

// Orchestrator is a picker option, not a model: a planner (Carbon or Argon,
// named in the option) plans and reviews, and Gemini 4 Hydrogen implements.
// Its steps report the role in their model id, so each step is priced as the
// model that did it.
var agOrchestratorOption = regexp.MustCompile(`(?i)^\s*orchestrator\s*\(\s*(carbon|argon)\s*\)\s*$`)

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

// parseAntigravityTranscript walks one transcript from the start, emitting one
// record per billed planner response.
func parseAntigravityTranscript(ctx context.Context, file string, data []byte) []AntigravityRecord {
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
	steps := readConversationDB(ctx, agConversationDB(file))
	selections := selectionTimeline(events, parsed)
	session := hashText(agSessionName(file))

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
		stepFinite := hasStepValue && stepValue.kind == 'f' && !math.IsInf(stepValue.f, 0) && !math.IsNaN(stepValue.f)
		if kind == "PLANNER_RESPONSE" && steps != nil && stepFinite {
			if n, ok := safeIntFloat(stepValue.f); ok {
				if found, ok := steps[n]; ok {
					turn = &found
				}
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
				delta.Input, delta.Cached, delta.Reasoning = exact.Uncached, exact.Cached, exact.Reasoning
				delta.Output = exact.TextOutput
				delta.CacheCreation = exact.CacheCreation
				delta.Total = delta.Input + delta.Cached + delta.Output + delta.Reasoning + delta.CacheCreation
				delta.BillableTotal, delta.ConversationCount = delta.Total, 1
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
			Totals: delta, Session: session, AccountingRevision: antigravityAccountingRevision,
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
			if turn.Usage != nil && turn.Usage.ResponseID != nil && *turn.Usage.ResponseID != "" {
				record.ResponseID = turn.Usage.ResponseID
			}
			if turn.Model != "" {
				record.ReportedModel = strPtr(turn.Model)
			}
			if turn.Usage != nil {
				record.ModelCode = turn.Usage.ModelCode
			}
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
	files, complete := listAntigravityTranscripts(geminiHome, known)
	live := map[string]bool{}
	for _, file := range files {
		live[file] = true
	}
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
	observations, corrected := 0, 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, record := range parseAntigravityTranscript(ctx, file, data) {
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
		FilesProcessed: len(files), Observations: observations, CorrectedRecords: corrected,
		AccountingRevision: antigravityAccountingRevision, SourceFiles: len(files), InventoryComplete: complete,
		MissingSources: missing, MissingSourceSessions: missingSessions,
		LastScan: stamp, NextScan: jsISOTime(now.Add(time.Hour)),
	}
	if err := putState(ctx, tx, "known-sources", uniqueStrings(append(known, files...))); err != nil {
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
// Verified 2026-11-13: https://aistudio.google.com/docs/pricing
var agRateCards = map[string][]agRate{
	"gemini-4-carbon":   {{mustDate("2026-11-10T00:00:00Z"), 2, 10, .1, 0}, {mustDate("2026-12-01T00:00:00Z"), 4, 20, .2, 0}},
	"gemini-4-hydrogen": {{mustDate("2026-11-12T00:00:00Z"), 1.5, 7.5, .075, 0}},
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
