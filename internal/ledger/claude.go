package ledger

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenusage/internal/model"
)

// Claude Code usage comes from the project JSONL transcripts; each request is
// identified by message and request ID so duplicated content blocks count once.

const claudeSchema = `PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS records(id TEXT PRIMARY KEY,payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS state(id TEXT PRIMARY KEY,payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS revisions(id TEXT,old_payload TEXT,new_payload TEXT,at TEXT);`

type claudeTotals struct {
	Input         int64 `json:"input_tokens"`
	Cached        int64 `json:"cached_input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	Output        int64 `json:"output_tokens"`
	Reasoning     int64 `json:"reasoning_output_tokens"`
	Total         int64 `json:"total_tokens"`
}

func (t claudeTotals) monotonicFrom(o claudeTotals) bool {
	return t.Input >= o.Input && t.Cached >= o.Cached && t.CacheCreation >= o.CacheCreation && t.Output >= o.Output && t.Reasoning >= o.Reasoning
}

type ClaudeRecord struct {
	ID                 string       `json:"id"`
	Session            string       `json:"session"`
	MessageKey         string       `json:"messageKey"`
	IsSidechain        bool         `json:"isSidechain"`
	Timestamp          string       `json:"timestamp"`
	Model              string       `json:"model"`
	SelectedModel      string       `json:"selectedModel"`
	ReportedModel      string       `json:"reportedModel"`
	Precision          string       `json:"precision"`
	OutputSplit        string       `json:"outputSplit"`
	TokenSource        string       `json:"tokenSource"`
	Cache5m            int64        `json:"cache5m"`
	Cache1h            int64        `json:"cache1h"`
	CacheDurationKnown bool         `json:"cacheDurationKnown"`
	Speed              string       `json:"speed"`
	Geo                string       `json:"geo"`
	Tier               string       `json:"tier"`
	Source             string       `json:"source"`
	SourceLine         int64        `json:"sourceLine"`
	WebSearch          int64        `json:"webSearch"`
	Totals             claudeTotals `json:"totals"`
	Excluded           string       `json:"excluded,omitempty"`
}

type claudeRate struct {
	Model                                         string
	ValidFrom                                     time.Time
	MinPromptTokens                               int64
	Input, Output, CacheRead, CacheWrite, Write1h float64
}

func mustDate(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

// Anthropic official Standard API rates (USD per million tokens), effective
// from each model's verified date. The 10/7 announcement explicitly changed
// Sonnet cache pricing; it is not back-applied. Models whose rate has never
// changed since release start at claudeLaunchRate, since no usage can predate
// the release itself.
var claudeRateCards = []claudeRate{
	{Model: "claude-fable-5-1", ValidFrom: claudeLaunchRate, Input: 10, Output: 50, CacheRead: .25, CacheWrite: 12.5, Write1h: 20},
	{Model: "claude-fable-5", ValidFrom: claudeLaunchRate, Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5, Write1h: 20},
	{Model: "claude-opus-5", ValidFrom: claudeLaunchRate, Input: 5, Output: 25, CacheRead: .5, CacheWrite: 6.25, Write1h: 10},
	{Model: "claude-opus-4-8", ValidFrom: claudeLaunchRate, Input: 5, Output: 25, CacheRead: .5, CacheWrite: 6.25, Write1h: 10},
	{Model: "claude-opus-4-7", ValidFrom: claudeLaunchRate, Input: 5, Output: 25, CacheRead: .5, CacheWrite: 6.25, Write1h: 10},
	{Model: "claude-opus-4-6", ValidFrom: mustDate("2026-02-05T00:00:00Z"), Input: 5, Output: 25, CacheRead: .5, CacheWrite: 6.25, Write1h: 10},
	{Model: "claude-sonnet-5", ValidFrom: claudeLaunchRate, Input: 2, Output: 10, CacheRead: .2, CacheWrite: 2.5, Write1h: 4},
	{Model: "claude-sonnet-4-6", ValidFrom: claudeLaunchRate, Input: 3, Output: 15, CacheRead: .3, CacheWrite: 3.75, Write1h: 6},
	{Model: "claude-haiku-4-5", ValidFrom: claudeLaunchRate, Input: 1, Output: 5, CacheRead: .1, CacheWrite: 1.25, Write1h: 2},
	{Model: "claude-opus-5-5", ValidFrom: mustDate("2026-09-22T00:00:00Z"), Input: 4, Output: 20, CacheRead: .2, CacheWrite: 5, Write1h: 8},
	{Model: "claude-sonnet-5-5", ValidFrom: mustDate("2026-10-07T00:00:00Z"), Input: 2, Output: 10, CacheRead: .1, CacheWrite: 2.5, Write1h: 4},
	{Model: "claude-haiku-5-5", ValidFrom: mustDate("2026-10-07T00:00:00Z"), Input: .1, Output: .5, CacheRead: .01, CacheWrite: .125, Write1h: .2},
	{Model: "claude-haiku-5-5", ValidFrom: mustDate("2026-10-07T00:00:00Z"), MinPromptTokens: 100001, Input: .5, Output: 2.5, CacheRead: .05, CacheWrite: .625, Write1h: 1},
}

var claudeLaunchRate = mustDate("2025-01-01T00:00:00Z")

// Fast mode is priced at 2× standard on these models only.
var claudeFastModels = map[string]bool{"claude-opus-5-5": true, "claude-opus-5": true, "claude-opus-4-8": true}

// claudeSyntheticModel marks messages Claude Code writes locally (such as
// usage-limit notices) without calling the API; they carry no tokens.
const claudeSyntheticModel = "<synthetic>"

var (
	claudeVersionDots = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-(\d)\.(\d)`)
	claudeDateSuffix  = regexp.MustCompile(`-\d{8}$`)
	claudeFamily      = regexp.MustCompile(`^claude-(opus|sonnet|haiku|fable)-\d`)
	claudeMessageID   = regexp.MustCompile(`^(msg_|resp_)`)
)

func claudeCanonical(name string) string {
	return claudeDateSuffix.ReplaceAllString(claudeVersionDots.ReplaceAllString(name, "claude-$1-$2-$3"), "")
}

func claudeRoots() []string {
	if value := os.Getenv("CLAUDE_CONFIG_DIR"); value != "" {
		var out []string
		for _, part := range strings.Split(value, ",") {
			if abs, err := filepath.Abs(strings.TrimSpace(part)); err == nil {
				out = append(out, abs)
			}
		}
		return out
	}
	home := os.Getenv("USERPROFILE")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return []string{filepath.Join(home, ".claude"), filepath.Join(home, ".config/claude")}
}

func OpenClaude(directory string, roots []string) (*Provider, error) {
	db, err := openLedger(directory, claudeSchema)
	if err != nil {
		return nil, err
	}
	if roots == nil {
		roots = claudeRoots()
	}
	p := &Provider{Name: ProviderClaude, Label: "Claude Code", ledger: db}
	p.scan = func(ctx context.Context) error { _, err := scanClaude(ctx, db, roots); return err }
	p.load = func(ctx context.Context) ([]Entry, error) { return loadClaude(ctx, db) }
	return p, nil
}

var errClaudeInvalid = errors.New("invalid Claude usage")

func claudeCount(v jsValue, ok bool) (int64, error) {
	if !ok || v.kind != 'f' {
		return 0, errClaudeInvalid
	}
	n, safe := safeIntFloat(v.f)
	if !safe || n < 0 {
		return 0, errClaudeInvalid
	}
	return n, nil
}

// optionalCount mirrors `value ?? 0` followed by the safe-integer check.
func optionalCount(parent jsValue, key string) (int64, error) {
	v, ok := parent.get(key)
	if !ok || v.kind == 'n' {
		return 0, nil
	}
	return claudeCount(v, true)
}

func jsString(v jsValue, ok bool) string {
	if !ok {
		return ""
	}
	switch v.kind {
	case 's':
		return v.s
	case 'f':
		return jsNumber(v.f)
	case 'b':
		return strconv.FormatBool(v.b)
	case 'n':
		return "null"
	}
	return v.stringify()
}

func truthyField(parent jsValue, key string) (jsValue, bool) {
	v, ok := parent.get(key)
	if !ok || !v.truthy() {
		return jsValue{}, false
	}
	return v, true
}

// orString mirrors `a || b || fallback` over string-valued fields.
func orString(fallback string, values ...jsValue) string {
	for _, value := range values {
		if value.truthy() {
			return jsString(value, true)
		}
	}
	return fallback
}

func parseClaudeEntry(e jsValue, file string, line int64) ([]ClaudeRecord, error) {
	if kind, _ := e.str("type"); kind != "assistant" {
		return nil, nil
	}
	m, ok := truthyField(e, "message")
	if !ok {
		return nil, nil
	}
	mainUsage, ok := truthyField(m, "usage")
	if !ok {
		return nil, nil
	}
	session := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	if value, ok := truthyField(e, "sessionId"); ok {
		session = jsString(value, true)
	}
	rawTime, _ := e.get("timestamp")
	at, valid := parseJSTime(jsString(rawTime, rawTime.kind == 's'))
	if !valid || rawTime.kind != 's' {
		return nil, errClaudeInvalid
	}
	requested, hasRequested := e.str("requestedModel")
	mID, hasMID := truthyField(m, "id")
	requestID, hasRequestID := truthyField(e, "requestId")
	uuid, hasUUID := truthyField(e, "uuid")
	timestampText := jsString(rawTime, true)

	build := func(u jsValue, modelName, suffix string) (ClaudeRecord, error) {
		input, err := claudeCount(u.get("input_tokens"))
		if err != nil {
			return ClaudeRecord{}, err
		}
		cache, err := optionalCount(u, "cache_read_input_tokens")
		if err != nil {
			return ClaudeRecord{}, err
		}
		write, err := optionalCount(u, "cache_creation_input_tokens")
		if err != nil {
			return ClaudeRecord{}, err
		}
		output, err := claudeCount(u.get("output_tokens"))
		if err != nil {
			return ClaudeRecord{}, err
		}
		split, hasSplit := truthyField(u, "cache_creation")
		one, five := int64(0), write
		if hasSplit {
			if one, err = optionalCount(split, "ephemeral_1h_input_tokens"); err != nil {
				return ClaudeRecord{}, err
			}
			if five, err = optionalCount(split, "ephemeral_5m_input_tokens"); err != nil {
				return ClaudeRecord{}, err
			}
		}
		if one+five != write {
			return ClaudeRecord{}, errClaudeInvalid
		}
		var thinking int64
		hasThinking := false
		if details, ok := u.get("output_tokens_details"); ok && details.kind != 'n' {
			if value, ok := details.get("thinking_tokens"); ok && value.kind != 'n' {
				if thinking, err = claudeCount(value, true); err != nil {
					return ClaudeRecord{}, err
				}
				if thinking > output {
					return ClaudeRecord{}, errClaudeInvalid
				}
				hasThinking = true
			}
		}
		var identity string
		switch {
		case hasMID && hasRequestID:
			identity = "request:" + jsString(mID, true) + "\x00" + jsString(requestID, true)
		case hasMID:
			id := jsString(mID, true)
			identity = "message:" + session + "\x00" + id
			if !claudeMessageID.MatchString(id) {
				identity += "\x00" + timestampText
			}
		default:
			identity = "uuid:" + session + "\x00" + jsString(uuid, hasUUID)
		}
		if !hasMID && !hasUUID {
			return ClaudeRecord{}, errClaudeInvalid
		}
		reported := claudeCanonical(modelName)
		selected := reported
		if suffix == "" && hasRequested && requested != "" && claudeFamily.MatchString(requested) {
			selected = claudeCanonical(requested)
		}
		messageID := jsString(uuid, hasUUID)
		if hasMID {
			messageID = jsString(mID, true)
		}
		selectedModel := modelName
		if suffix == "" && hasRequested && requested != "" {
			selectedModel = requested
		}
		outputSplit := "combined"
		if hasThinking {
			outputSplit = "split"
		}
		mu, _ := m.get("usage")
		webSearch := int64(0)
		if tools, ok := u.get("server_tool_use"); ok && tools.kind == 'o' {
			if value, ok := tools.get("web_search_requests"); ok && value.truthy() && value.kind == 'f' {
				webSearch = int64(value.f)
			}
		}
		pick := func(key, fallback string) string {
			a, _ := u.get(key)
			b, _ := mu.get(key)
			return orString(fallback, a, b)
		}
		sidechain, _ := e.get("isSidechain")
		return ClaudeRecord{
			ID: hashText(identity + suffix), Session: hashText(session), MessageKey: hashText(session + "\x00" + messageID + suffix),
			IsSidechain: sidechain.truthy(), Timestamp: jsISOTime(at), Model: selected, SelectedModel: selectedModel, ReportedModel: modelName,
			Precision: "recorded", OutputSplit: outputSplit, TokenSource: "claude-jsonl",
			Cache5m: five, Cache1h: one, CacheDurationKnown: hasSplit || write == 0,
			Speed: pick("speed", "standard"), Geo: pick("inference_geo", "global"), Tier: pick("service_tier", "standard"),
			Source: hashText(file), SourceLine: line, WebSearch: webSearch,
			Totals: claudeTotals{Input: input, Cached: cache, CacheCreation: write, Output: output - thinking, Reasoning: thinking, Total: input + cache + write + output},
		}, nil
	}
	modelValue, _ := m.get("model")
	modelName := orString("unknown", modelValue)
	first, err := build(mainUsage, modelName, "")
	if err != nil {
		return nil, err
	}
	out := []ClaudeRecord{first}
	if iterations, ok := mainUsage.get("iterations"); ok && iterations.kind == 'a' {
		advisor, _ := e.get("advisorModel")
		for index, item := range iterations.items {
			if kind, _ := item.str("type"); kind != "advisor_message" {
				continue
			}
			itemModel, _ := item.get("model")
			record, err := build(item, orString("unknown", itemModel, advisor), ":advisor:"+strconv.Itoa(index))
			if err != nil {
				return nil, err
			}
			out = append(out, record)
		}
	}
	return out, nil
}

type claudeScan struct {
	SourceFiles          int      `json:"sourceFiles"`
	InventoryComplete    bool     `json:"inventoryComplete"`
	MissingSources       int      `json:"missingSources"`
	Observations         int      `json:"observations"`
	UniqueSourceRequests int      `json:"uniqueSourceRequests"`
	DuplicateLines       int      `json:"duplicateLines"`
	InvalidLines         int      `json:"invalidLines"`
	Issues               []string `json:"issues"`
	Inserted             int      `json:"inserted"`
	Updated              int      `json:"updated"`
	LastScan             string   `json:"lastScan"`
	NextScan             string   `json:"nextScan"`
}

func scanClaude(ctx context.Context, db *ledgerDB, homes []string) (claudeScan, error) {
	var files []string
	var issues []string
	complete := true
	seenHomes := map[string]bool{}
	for _, home := range homes {
		if seenHomes[home] {
			continue
		}
		seenHomes[home] = true
		root := filepath.Join(home, "projects")
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				complete = false
				issues = append(issues, "有來源資料夾無法讀取")
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() && strings.HasSuffix(path, ".jsonl") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			complete = false
		}
	}
	files = uniqueStrings(files)
	var raw []ClaudeRecord
	invalidLines, duplicateLines := 0, 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil || len(data) == 0 {
			continue
		}
		// Freeze each source prefix; an unterminated last line is retried next scan.
		lines := bytes.Split(data, []byte("\n"))
		// The final element is either empty (terminated file) or an unfinished line.
		lines = lines[:len(lines)-1]
		for index, text := range lines {
			text = bytes.TrimSuffix(text, []byte("\r"))
			if !bytes.Contains(text, []byte(`"usage"`)) {
				continue
			}
			value, err := parseJS(text)
			if err != nil {
				invalidLines++
				continue
			}
			records, err := parseClaudeEntry(value, file, int64(index+1))
			if err != nil {
				invalidLines++
				continue
			}
			raw = append(raw, records...)
		}
	}

	rows, err := db.records(ctx)
	if err != nil {
		return claudeScan{}, err
	}
	existing := make([]ClaudeRecord, 0, len(rows))
	for _, row := range rows {
		var record ClaudeRecord
		if err := jsonUnmarshal(row.Payload, &record); err != nil {
			return claudeScan{}, err
		}
		existing = append(existing, record)
	}
	// Drop parent replays in sidechains, without suppressing their own responses.
	parents := map[string]bool{}
	for _, list := range [][]ClaudeRecord{raw, existing} {
		for _, r := range list {
			if !r.IsSidechain {
				parents[r.MessageKey] = true
			}
		}
	}
	merged := map[string]ClaudeRecord{}
	var order []string
	for _, r := range raw {
		if r.IsSidechain && parents[r.MessageKey] {
			duplicateLines++
			continue
		}
		prior, ok := merged[r.ID]
		if !ok {
			merged[r.ID] = r
			order = append(order, r.ID)
			continue
		}
		duplicateLines++
		if r.Totals.monotonicFrom(prior.Totals) {
			if prior.Timestamp < r.Timestamp {
				r.Timestamp = prior.Timestamp
			}
			merged[r.ID] = r
		} else if !prior.Totals.monotonicFrom(r.Totals) {
			issues = append(issues, "相同請求出現互相衝突的分類計數，保留既有完整值")
		}
	}
	byID := make(map[string]ClaudeRecord, len(existing))
	for _, r := range existing {
		byID[r.ID] = r
	}
	// Read state before the write transaction; the ledger has one connection.
	var known []string
	if _, err := db.state(ctx, "sources", &known); err != nil {
		return claudeScan{}, err
	}
	now := time.Now()
	stamp := jsISOTime(now)
	inserted, updated := 0, 0
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return claudeScan{}, err
	}
	defer tx.Rollback()
	for _, id := range order {
		r := merged[id]
		next, err := marshalJS(r)
		if err != nil {
			return claudeScan{}, err
		}
		if before, ok := byID[id]; ok {
			if before.Totals.Total > r.Totals.Total {
				issues = append(issues, "來源請求計數小於已儲存值，保留帳本待核對")
				continue
			}
			if before.Totals != r.Totals {
				old, _ := marshalJS(before)
				if _, err := tx.ExecContext(ctx, `INSERT INTO revisions VALUES(?,?,?,?)`, id, old, next, stamp); err != nil {
					return claudeScan{}, err
				}
				updated++
			} else if before == r {
				continue
			}
		} else {
			inserted++
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO records VALUES(?,?)`, id, next); err != nil {
			return claudeScan{}, err
		}
	}
	// Keep the original sidechain row for audit, but exclude proven parent replays.
	for _, old := range existing {
		if !old.IsSidechain || !parents[old.MessageKey] || old.Excluded != "" {
			continue
		}
		if current, ok := merged[old.ID]; ok && !current.IsSidechain {
			continue
		}
		nextRecord := old
		nextRecord.Excluded = "parent-sidechain-replay"
		oldText, _ := marshalJS(old)
		nextText, _ := marshalJS(nextRecord)
		if _, err := tx.ExecContext(ctx, `INSERT INTO revisions VALUES(?,?,?,?)`, old.ID, oldText, nextText, stamp); err != nil {
			return claudeScan{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO records VALUES(?,?)`, old.ID, nextText); err != nil {
			return claudeScan{}, err
		}
	}
	live := map[string]bool{}
	var liveList []string
	for _, file := range files {
		hash := hashText(file)
		live[hash] = true
		liveList = append(liveList, hash)
	}
	saved := map[string]bool{}
	for _, r := range existing {
		saved[r.Source] = true
	}
	for _, r := range merged {
		saved[r.Source] = true
	}
	missing := 0
	for source := range saved {
		if !live[source] {
			missing++
		}
	}
	scan := claudeScan{
		SourceFiles: len(files), InventoryComplete: complete, MissingSources: missing, Observations: len(raw),
		UniqueSourceRequests: len(merged), DuplicateLines: duplicateLines, InvalidLines: invalidLines,
		Issues: uniqueStrings(issues), Inserted: inserted, Updated: updated,
		LastScan: stamp, NextScan: jsISOTime(now.Add(time.Hour)),
	}
	if scan.Issues == nil {
		scan.Issues = []string{}
	}
	if err := putState(ctx, tx, "sources", uniqueStrings(append(known, liveList...))); err != nil {
		return claudeScan{}, err
	}
	if err := putState(ctx, tx, "scan", scan); err != nil {
		return claudeScan{}, err
	}
	return scan, tx.Commit()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func priceClaude(r ClaudeRecord, overrides []DatedOverride) Price {
	at, _ := parseJSTime(r.Timestamp)
	prompt := r.Totals.Input + r.Totals.Cached + r.Totals.CacheCreation
	manual := pickOverride(overrides, r.Model, taipeiDay(at), prompt, claudeCanonical)
	var card *claudeRate
	canonical := claudeCanonical(r.Model)
	for index := range claudeRateCards {
		c := &claudeRateCards[index]
		if c.Model != canonical || c.ValidFrom.After(at) || prompt < c.MinPromptTokens {
			continue
		}
		if card == nil || c.ValidFrom.After(card.ValidFrom) || (c.ValidFrom.Equal(card.ValidFrom) && c.MinPromptTokens > card.MinPromptTokens) {
			card = c
		}
	}
	if manual == nil && card == nil {
		return unpriced("unverified_rate", "模型或使用日期尚無已核實費率")
	}
	if manual == nil && r.Speed != "standard" && r.Speed != "fast" {
		return unpriced("unknown_speed", "未知速度模式，未猜測加價")
	}
	if manual == nil && r.Speed == "fast" && !claudeFastModels[canonical] {
		return unpriced("unverified_fast", "此模型的快速模式費率尚未核實")
	}
	if manual == nil && r.Geo != "global" && r.Geo != "us" {
		return unpriced("unknown_geo", "未知推論地區，未猜測加價")
	}
	if !r.CacheDurationKnown {
		return unpriced("cache_duration_unknown", "缺少快取寫入時效，無法區分 5 分鐘與 1 小時費率")
	}
	var input, output, read, write5m, write1h float64
	if manual != nil {
		input, output, read, write5m = manual.Input, manual.Output, manual.CacheRead, manual.CacheWrite
		if manual.CacheWrite1h != nil {
			write1h = *manual.CacheWrite1h
		}
	} else {
		factor := 1.0
		if r.Speed == "fast" {
			factor *= 2
		}
		if r.Geo == "us" {
			factor *= 1.1
		}
		input, output, read, write5m, write1h = card.Input*factor, card.Output*factor, card.CacheRead*factor, card.CacheWrite*factor, card.Write1h*factor
	}
	t := r.Totals
	return Price{
		Priced: true, RegularNano: usdNano(t.Input, input), CachedNano: usdNano(t.Cached, read),
		CacheWriteNano: usdNano(r.Cache5m, write5m) + usdNano(r.Cache1h, write1h),
		OutputNano:     usdNano(t.Output+t.Reasoning, output),
	}
}

func loadClaude(ctx context.Context, db *ledgerDB) ([]Entry, error) {
	rows, err := db.records(ctx)
	if err != nil {
		return nil, err
	}
	overrides := db.overrides(ctx)
	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		var r ClaudeRecord
		if err := jsonUnmarshal(row.Payload, &r); err != nil {
			return nil, err
		}
		if r.Excluded != "" || r.Model == claudeSyntheticModel {
			continue
		}
		at, ok := parseJSTime(r.Timestamp)
		if !ok {
			continue
		}
		mode := model.ModeStandard
		if r.Speed == "fast" {
			mode = model.ModeFast
		}
		t := r.Totals
		out = append(out, Entry{
			ID: "claude:" + r.ID, Session: r.Session, Timestamp: at, Model: canonicalModel(r.Model), Mode: mode,
			Precision: r.Precision, TokenSource: r.TokenSource,
			Usage: model.TokenUsage{
				Input: t.Input + t.Cached + t.CacheCreation, CachedInput: t.Cached, CacheWriteInput: t.CacheCreation,
				Output: t.Output + t.Reasoning, ReasoningOutput: t.Reasoning, Total: t.Total,
			},
			Price: priceClaude(r, overrides),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}
