package limits

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestObserveFoldsReadingsOfOneWindow(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	reset := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	base := Observation{Provider: Codex, Key: "codex:300", WindowMinutes: 300, ResetsAt: reset}
	readings := []struct {
		percent float64
		drift   time.Duration
		at      time.Time
	}{
		{20, 0, reset.Add(-4 * time.Hour)},
		{55, 40 * time.Second, reset.Add(-2 * time.Hour)},
		// A backfill can deliver an older reading last.
		{10, -30 * time.Second, reset.Add(-4*time.Hour - time.Minute)},
	}
	for _, r := range readings {
		o := base
		o.UsedPercent, o.ResetsAt, o.ObservedAt = r.percent, reset.Add(r.drift), r.at
		if err := st.Observe(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	next := base
	next.UsedPercent, next.ResetsAt, next.ObservedAt = 5, reset.Add(5*time.Hour), reset.Add(time.Hour)
	if err := st.Observe(ctx, next); err != nil {
		t.Fatal(err)
	}
	// Untouched quotas report a sliding reset and must not become windows.
	for i := 0; i < 3; i++ {
		idle := base
		idle.UsedPercent, idle.ObservedAt = 0, reset.Add(time.Duration(i)*10*time.Minute)
		idle.ResetsAt = idle.ObservedAt.Add(5 * time.Hour)
		if err := st.Observe(ctx, idle); err != nil {
			t.Fatal(err)
		}
	}
	windows, err := st.Windows(ctx, Codex, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}
	w := windows[1]
	if w.MaxPercent != 55 || w.LastPercent != 55 || !w.FirstSeen.Equal(reset.Add(-4*time.Hour-time.Minute)) {
		t.Fatalf("folded window = %+v", w)
	}
}

func fakeIDToken(t *testing.T, email, account, user, plan string) string {
	t.Helper()
	claims, _ := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]string{
		"chatgpt_account_id": account, "chatgpt_user_id": user, "chatgpt_plan_type": plan,
	}})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func writeAuth(t *testing.T, home, email, account, user, plan string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"tokens": map[string]string{"id_token": fakeIDToken(t, email, account, user, plan), "access_token": "secret"}})
	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rateLine(at time.Time, plan string, primary, secondary *codexWindow) string {
	limits := map[string]any{"limit_id": "codex", "plan_type": plan, "primary": primary, "secondary": secondary}
	data, _ := json.Marshal(map[string]any{"timestamp": at.Format(time.RFC3339Nano), "type": "event_msg",
		"payload": map[string]any{"type": "token_count", "info": nil, "rate_limits": limits}})
	return string(data) + "\n"
}

func TestCodexCollectorAttributesAccountsFromTrackingStart(t *testing.T) {
	st := openTestStore(t)
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "08")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	now := start
	collector := &CodexCollector{Store: st, Homes: func() ([]string, error) { return []string{home}, nil }, Now: func() time.Time { return now }}
	// Every account has its own weekly reset.
	weekly := func(percent float64, hours int) *codexWindow {
		return &codexWindow{UsedPercent: percent, WindowMinutes: 10080, ResetsAt: start.Add(time.Duration(hours) * time.Hour).Unix()}
	}
	fiveHour := &codexWindow{UsedPercent: 12, WindowMinutes: 300, ResetsAt: start.Add(3 * time.Hour).Unix()}
	path := filepath.Join(dir, "rollout-a.jsonl")
	// Before tracking: a team account, and b, identified later by its window.
	history := rateLine(start.Add(-time.Hour), "team", fiveHour, weekly(30, 50)) +
		rateLine(start.Add(-50*time.Minute), "plus", nil, weekly(20, 90)) +
		// Read at 0%: its window slides and names no account; the line before does.
		rateLine(start.Add(-45*time.Minute), "plus", nil, &codexWindow{WindowMinutes: 10080, ResetsAt: start.Add(7*24*time.Hour - 45*time.Minute).Unix()}) +
		`{"timestamp":"x","type":"response_item","payload":{"content":"` + strings.Repeat("y", 300<<10) + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(history), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAuth(t, home, "a@example.test", "acct-a", "user-a", "plus")
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// After tracking began: another account, plus a partial trailing line.
	now = start.Add(time.Hour)
	writeAuth(t, home, "b@example.test", "acct-b", "user-b", "plus")
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	file.WriteString(rateLine(start.Add(30*time.Minute), "plus", nil, weekly(40, 72)))
	file.WriteString(`{"timestamp":"partial`)
	file.Close()
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = start.Add(2 * time.Hour)
	file, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	file.WriteString(`","type":"event_msg"}` + "\n" + rateLine(start.Add(90*time.Minute), "plus", nil, weekly(45, 90)))
	file.Close()
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	windows, err := st.Windows(context.Background(), Codex, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Window{}
	for _, w := range windows {
		got[w.Account+"|"+w.Key] = w
	}
	team := HistoryPrefix + "team:" + strconv.FormatInt(start.Add(50*time.Hour).Unix(), 10)
	if w := got[team+"|codex:300"]; w.MaxPercent != 12 || w.Plan != "team" {
		t.Fatalf("history 5-hour window = %+v (all: %+v)", w, windows)
	}
	if w := got[team+"|codex:10080"]; w.MaxPercent != 30 {
		t.Fatalf("history weekly window = %+v", w)
	}
	// 06:30 precedes the 07:00 switch to b, so it belongs to a.
	if w := got["acct-a/user-a|codex:10080"]; w.MaxPercent != 40 {
		t.Fatalf("account a weekly window = %+v (all: %+v)", w, windows)
	}
	// b's earlier unknown reading of the same window is adopted and merged.
	if w := got["acct-b/user-b|codex:10080"]; w.MaxPercent != 45 || !w.FirstSeen.Equal(start.Add(-50*time.Minute)) || len(windows) != 4 {
		t.Fatalf("account b weekly window = %+v (all: %+v)", w, windows)
	}
	accounts, _ := st.Accounts(context.Background(), Codex)
	if len(accounts) != 2 || accounts[0].Email != "b@example.test" {
		t.Fatalf("accounts = %+v", accounts)
	}
	// Usage is attributed per minute: b's adopted 05:10 and 05:15 (read at 0%)
	// and its 07:30 are split by a's 06:30, and the team account keeps its own.
	usage := map[string][]TimeRange{}
	for _, account := range []string{"acct-a/user-a", "acct-b/user-b", team} {
		ranges, err := st.CodexUsageRanges(context.Background(), account, start.Add(-2*time.Hour), start.Add(3*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		usage[account] = ranges
	}
	minute := func(offset time.Duration) TimeRange {
		return TimeRange{Since: start.Add(offset), Until: start.Add(offset + time.Minute)}
	}
	want := map[string][]TimeRange{
		"acct-a/user-a": {minute(30 * time.Minute)},
		"acct-b/user-b": {{Since: start.Add(-50 * time.Minute), Until: start.Add(-44 * time.Minute)}, minute(90 * time.Minute)},
		team:            {minute(-time.Hour)},
	}
	if fmt.Sprint(usage) != fmt.Sprint(want) {
		t.Fatalf("usage ranges = %v, want %v", usage, want)
	}
}

// Codex sometimes rewrites old conversations in place. Reading them again
// must keep windows that already belong to a known account with that account
// instead of splitting them off as unknown accounts.
func TestCodexRereadKeepsWindowsOfKnownAccounts(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "03")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	collector := &CodexCollector{Store: st, Homes: func() ([]string, error) { return []string{home}, nil }, Now: func() time.Time { return start }}
	before := start.Add(-5 * 24 * time.Hour)
	fiveHour := &codexWindow{UsedPercent: 40, WindowMinutes: 300, ResetsAt: before.Add(3 * time.Hour).Unix()}
	weekly := &codexWindow{UsedPercent: 60, WindowMinutes: 10080, ResetsAt: before.Add(50 * time.Hour).Unix()}
	path := filepath.Join(dir, "rollout-old.jsonl")
	content := rateLine(before, "team", fiveHour, weekly) + rateLine(before.Add(10*time.Minute), "team", fiveHour, weekly)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	unknown := HistoryPrefix + "team:" + strconv.FormatInt(weekly.ResetsAt, 10)
	const known = "acct-t/user-t"
	// The history is assigned to its account directly.
	for _, stmt := range []string{
		`UPDATE windows SET account=? WHERE account=?`,
		`UPDATE readings SET account=? WHERE account=?`,
		`UPDATE latest SET account=? WHERE account=?`,
		`UPDATE codex_usage_accounts SET account=? WHERE account=?`,
	} {
		if _, err := st.db.ExecContext(ctx, stmt, known, unknown); err != nil {
			t.Fatal(err)
		}
	}
	// The rewrite is caught half-written, then complete.
	if err := os.WriteFile(path, []byte(content[:len(content)/2]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	windows, err := st.Windows(ctx, Codex, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	for _, w := range windows {
		if w.Account != known {
			t.Fatalf("window split off after reread: %+v", w)
		}
	}
	ranges, err := st.CodexUsageRanges(ctx, known, before.Add(-time.Hour), before.Add(time.Hour))
	if err != nil || len(ranges) == 0 {
		t.Fatalf("usage of the known account after reread = %v, %v", ranges, err)
	}
	if ranges, _ := st.CodexUsageRanges(ctx, unknown, before.Add(-time.Hour), before.Add(time.Hour)); len(ranges) != 0 {
		t.Fatalf("usage moved back to the unknown account: %v", ranges)
	}
}

func TestAdoptHistoryKeepsTheAccountsNewerLatestReading(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	observe := func(account string, reset time.Time, percent float64, at time.Time) {
		t.Helper()
		if err := st.Observe(ctx, Observation{Provider: Codex, Account: account, Plan: "plus", Key: "codex:10080",
			WindowMinutes: 10080, UsedPercent: percent, ResetsAt: reset, ObservedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	observe(HistoryPrefix+"plus:1", now.Add(-24*time.Hour), 50, now.Add(-48*time.Hour))
	observe("acct/user", now.Add(72*time.Hour), 96, now)
	if err := st.adoptHistory(ctx, "acct/user", "codex:10080", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if latest, _ := st.Latest(ctx, Codex, now); len(latest) != 1 || latest[0].LastPercent != 96 {
		t.Fatalf("latest = %+v", latest)
	}
}
func TestWindowsEndAtEarlyResets(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	week := 7 * 24 * time.Hour
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	observe := func(provider, key string, minutes int, reset, at time.Time, percent float64) {
		t.Helper()
		if err := st.Observe(ctx, Observation{Provider: provider, Account: "a", Key: key, WindowMinutes: minutes,
			UsedPercent: percent, ResetsAt: reset, ObservedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	// Codex moves the reset time: the old window ends where the new one begins.
	reset := start.Add(3 * 24 * time.Hour)
	observe(Codex, "codex:10080", 10080, start.Add(week), start.Add(time.Hour), 60)
	observe(Codex, "codex:10080", 10080, reset.Add(week), reset.Add(time.Hour), 5)
	// A natural rollover right at the reset is not an early reset.
	observe(Codex, "codex:300", 300, start.Add(5*time.Hour), start.Add(time.Hour), 30)
	observe(Codex, "codex:300", 300, start.Add(10*time.Hour), start.Add(6*time.Hour), 10)
	windows, _ := st.Windows(ctx, Codex, 10)
	ended := map[string]time.Time{}
	for _, w := range windows {
		ended[w.Key+"|"+w.ResetsAt.Format(time.RFC3339)] = w.EndedAt
	}
	if got := ended["codex:10080|"+start.Add(week).Format(time.RFC3339)]; !got.Equal(reset) {
		t.Fatalf("early reset weekly ended %v, want %v (all: %+v)", got, reset, windows)
	}
	for key, at := range ended {
		if !strings.HasPrefix(key, "codex:10080|"+start.Add(week).Format(time.RFC3339)) && !at.IsZero() {
			t.Fatalf("%s ended early at %v", key, at)
		}
	}
	// Claude keeps the reset time and zeroes the usage; any other fall is not a reset.
	claudeReset := start.Add(week)
	observe(Claude, "seven_day", 10080, claudeReset, start.Add(time.Hour), 40)
	observe(Claude, "seven_day", 10080, claudeReset, start.Add(2*time.Hour), 10)
	observe(Claude, "seven_day", 10080, claudeReset, start.Add(3*time.Hour), 0)
	observe(Claude, "seven_day", 10080, claudeReset, start.Add(4*time.Hour), 4)
	windows, _ = st.Windows(ctx, Claude, 10)
	if len(windows) != 2 {
		t.Fatalf("claude windows = %+v", windows)
	}
	after, before := windows[0], windows[1]
	if !before.EndedAt.Equal(start.Add(3*time.Hour)) || before.MaxPercent != 40 || !before.StartedAt.IsZero() {
		t.Fatalf("before the reset = %+v", before)
	}
	if !after.StartedAt.Equal(start.Add(3*time.Hour)) || !after.EndedAt.IsZero() || after.MaxPercent != 4 {
		t.Fatalf("after the reset = %+v", after)
	}
}

func TestLatestKeepsLimitsOfTheNewestReading(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	observe := func(account, key string, minutes int, reset, at time.Time, percent float64) {
		t.Helper()
		if err := st.Observe(ctx, Observation{Provider: Codex, Account: account, Key: key, WindowMinutes: minutes,
			UsedPercent: percent, ResetsAt: reset, ObservedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	// b's newest reading had both limits; both have reset since.
	observe("b", "codex:10080", 10080, now.Add(-time.Hour), now.Add(-4*24*time.Hour), 100)
	observe("b", "codex:300", 300, now.Add(-4*24*time.Hour+3*time.Hour), now.Add(-4*24*time.Hour), 80)
	// a's newer readings no longer carry the 5-hour limit.
	observe("a", "codex:300", 300, now.Add(-100*24*time.Hour), now.Add(-100*24*time.Hour-time.Hour), 50)
	observe("a", "codex:10080", 10080, now.Add(5*24*time.Hour), now.Add(-time.Hour), 96)
	latest, err := st.Latest(ctx, Codex, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, w := range latest {
		got[w.Account+"|"+w.Key] = w.LastPercent
	}
	want := map[string]float64{"a|codex:10080": 96, "b|codex:10080": 0, "b|codex:300": 0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("latest = %v, want %v", got, want)
	}
}

func TestClaudeCollectorRefreshesAndPreservesLogin(t *testing.T) {
	st := openTestStore(t)
	dir := t.TempDir()
	now := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	login := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": "old-access", "refreshToken": "old-refresh", "expiresAt": now.Add(5 * time.Minute).UnixMilli(),
			"scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "pro",
		},
		"otherTool": map[string]any{"keep": true},
	}
	data, _ := json.Marshal(login)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var refreshBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &refreshBody)
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":28800}`)
		case "/usage":
			if r.Header.Get("Authorization") != "Bearer new-access" {
				http.Error(w, "stale token", http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"five_hour":{"utilization":35.0,"resets_at":"2026-11-08T10:20:00.1+00:00"},"seven_day":{"utilization":82.0,"resets_at":"2026-11-08T07:00:00+00:00"},"seven_day_opus":null}`)
		case "/profile":
			fmt.Fprint(w, `{"account":{"uuid":"u1","email":"me@example.test","has_claude_pro":true},"organization":{"uuid":"o1"}}`)
		}
	}))
	defer server.Close()
	collector := &ClaudeCollector{Store: st, ConfigDir: dir, Now: func() time.Time { return now },
		UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token", Profile: server.URL + "/profile"}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshBody["grant_type"] != "refresh_token" || refreshBody["refresh_token"] != "old-refresh" ||
		refreshBody["client_id"] != claudeClientID || refreshBody["scope"] != "user:inference user:profile" {
		t.Fatalf("refresh request = %+v", refreshBody)
	}
	var saved map[string]any
	data, _ = os.ReadFile(filepath.Join(dir, ".credentials.json"))
	json.Unmarshal(data, &saved)
	oauth := saved["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != "new-access" || oauth["refreshToken"] != "new-refresh" || oauth["subscriptionType"] != "pro" ||
		int64(oauth["expiresAt"].(float64)) != now.Add(8*time.Hour).UnixMilli() || saved["otherTool"] == nil {
		t.Fatalf("saved login = %s", data)
	}
	windows, _ := st.Windows(context.Background(), Claude, 10)
	if len(windows) != 2 || windows[0].Key != "five_hour" || windows[0].LastPercent != 35 || windows[0].Account != "u1/o1" || windows[0].Plan != "Pro" {
		t.Fatalf("windows = %+v", windows)
	}
	if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(context.Background()); err != ErrClaudeSignedOut {
		t.Fatalf("signed-out error = %v", err)
	}
}

// limits[] carries every meter the plan has, including a model's own weekly
// limit on Max plans, without the client knowing its name in advance.
func TestClaudeCollectorReadsServerLimitRows(t *testing.T) {
	st := openTestStore(t)
	dir := t.TempDir()
	now := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	data, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "a", "refreshToken": "r", "expiresAt": now.Add(6 * time.Hour).UnixMilli()}})
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"five_hour":{"utilization":1,"resets_at":"2026-11-08T09:00:00Z"},"limits":[
			{"kind":"session","group":"session","percent":35,"resets_at":"2026-11-08T10:20:00Z","scope":null},
			{"kind":"weekly_all","group":"weekly","percent":82,"resets_at":"2026-11-14T07:00:00Z","scope":null},
			{"kind":"weekly_scoped","group":"weekly","percent":40,"resets_at":"2026-11-14T07:00:00Z","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}},
			{"kind":"weekly_scoped","group":"weekly","percent":null,"resets_at":null,"scope":{"model":null,"surface":{"id":"cowork","display_name":"Cowork"}}}]}`)
	}))
	defer server.Close()
	collector := &ClaudeCollector{Store: st, ConfigDir: dir, Now: func() time.Time { return now },
		UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token", Profile: server.URL + "/profile"}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.Latest(context.Background(), Claude, now)
	got := map[string]Window{}
	for _, w := range latest {
		got[w.Key] = w
	}
	if len(latest) != 3 || got["five_hour"].LastPercent != 35 || got["seven_day"].LastPercent != 82 ||
		got["weekly_scoped:fable"].Label != "Fable" || got["weekly_scoped:fable"].WindowMinutes != 10080 || got["weekly_scoped:fable"].LastPercent != 40 {
		t.Fatalf("latest = %+v", latest)
	}
}

// A saved expiry can outlive the token, for example after another program
// refreshed the login. A 401 then forces one refresh and a retry.
func TestClaudeCollectorRefreshesAfterUnauthorized(t *testing.T) {
	st := openTestStore(t)
	dir := t.TempDir()
	now := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	data, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "revoked", "refreshToken": "r1", "expiresAt": now.Add(6 * time.Hour).UnixMilli(),
	}})
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshes++
			fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
		case "/usage":
			if r.Header.Get("Authorization") != "Bearer fresh" {
				http.Error(w, "revoked", http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"five_hour":{"utilization":1,"resets_at":"2026-11-08T10:00:00Z"}}`)
		default:
			http.Error(w, "no profile", http.StatusNotFound)
		}
	}))
	defer server.Close()
	collector := &ClaudeCollector{Store: st, ConfigDir: dir, Now: func() time.Time { return now },
		UsageURL: server.URL + "/usage", TokenURL: server.URL + "/token", Profile: server.URL + "/profile"}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saved map[string]map[string]any
	data, _ = os.ReadFile(filepath.Join(dir, ".credentials.json"))
	json.Unmarshal(data, &saved)
	if refreshes != 1 || saved["claudeAiOauth"]["accessToken"] != "fresh" || saved["claudeAiOauth"]["refreshToken"] != "r1" {
		t.Fatalf("refreshes=%d saved=%s", refreshes, data)
	}
}

// The quota summary carries each group's weekly and 5-hour limits together.
func TestAntigravityCollectorReadsBothLimitsFromSummary(t *testing.T) {
	st := openTestStore(t)
	gemini := t.TempDir()
	daemon := filepath.Join(gemini, "antigravity", "daemon")
	if err := os.MkdirAll(daemon, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case antigravityStatusPath:
			fmt.Fprint(w, `{"userStatus":{"email":"me@example.test","planStatus":{"planInfo":{"planName":"Pro"}}}}`)
		case antigravitySummaryPath:
			fmt.Fprint(w, `{"response":{"groups":[
				{"displayName":"Claude and GPT models","buckets":[
					{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.92,"resetTime":"2026-11-15T00:00:00Z"},
					{"bucketId":"3p-5h","window":"5h","remainingFraction":0.9,"resetTime":"2026-11-08T16:10:00Z"}]},
				{"displayName":"Gemini 4 Argon","buckets":[
					{"bucketId":"gemini-argon-wtu-weekly","window":"weekly","remainingFraction":0,"resetTime":"2026-11-14T06:00:00Z"}]}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	data, _ := json.Marshal(map[string]any{"httpPort": server.Listener.Addr().(*net.TCPAddr).Port, "csrfToken": "csrf"})
	if err := os.WriteFile(filepath.Join(daemon, "hub_1.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	collector := &AntigravityCollector{Store: st, GeminiHome: gemini, Now: func() time.Time { return now }}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.Latest(context.Background(), Antigravity, now)
	got := map[string]Window{}
	for _, w := range latest {
		got[w.Key] = w
	}
	if len(latest) != 3 || got["3p-weekly"].WindowMinutes != 10080 || got["3p-weekly"].LastPercent != 8 ||
		got["3p-5h"].WindowMinutes != 300 || got["3p-5h"].Label != "Claude and GPT models" || got["gemini-argon-wtu-weekly"].LastPercent != 100 {
		t.Fatalf("latest = %+v", latest)
	}
}

func TestAntigravityCollectorReadsDiscoveryAndGroupsVariants(t *testing.T) {
	st := openTestStore(t)
	gemini := t.TempDir()
	daemon := filepath.Join(gemini, "antigravity", "daemon")
	if err := os.MkdirAll(daemon, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	// First Claude Opus is out of its weekly quota; later only its 5-hour
	// window is reported, while Argon's weekly window nears its reset.
	models := `{"label":"Gemini 4 Argon (High)","modelId":"gemini-4-argon-high","quotaInfo":{"remainingFraction":0.25,"resetTime":"2026-11-10T06:00:00Z"}},
			{"label":"Gemini 4 Argon (Low)","modelId":"gemini-4-argon-low","quotaInfo":{"remainingFraction":0.25,"resetTime":"2026-11-10T06:00:00Z"}},
			{"label":"Claude Opus 5.5 (High)","modelId":"claude-opus-5-5-high","quotaInfo":{"resetTime":"2026-11-12T09:00:00Z"}},
			{"label":"Tab","modelId":"tab_flash_lite_preview","quotaInfo":{"remainingFraction":1}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Codeium-Csrf-Token") != "csrf" || r.URL.Path != antigravityStatusPath {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"userStatus":{"email":"me@example.test","planStatus":{"planInfo":{"planName":"Pro"}},"cascadeModelConfigData":{"clientModelConfigs":[`+models+`]}}}`)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	write := func(name string, port int, token string, age time.Duration) {
		data, _ := json.Marshal(map[string]any{"httpPort": port, "csrfToken": token})
		path := filepath.Join(daemon, name)
		os.WriteFile(path, data, 0o600)
		os.Chtimes(path, now.Add(-age), now.Add(-age))
	}
	write("hub_1.json", port, "csrf", time.Hour)
	write("hub_2.json", 1, "stale", 0)
	collector := &AntigravityCollector{Store: st, GeminiHome: gemini, Now: func() time.Time { return now }}
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	windows, _ := st.Windows(context.Background(), Antigravity, 10)
	got := map[string]Window{}
	for _, w := range windows {
		got[w.Key] = w
	}
	if len(windows) != 2 || got["gemini-4-argon:10080"].LastPercent != 75 || got["gemini-4-argon:10080"].Label != "Gemini 4 Argon" ||
		got["claude-opus-5-5:10080"].LastPercent != 100 {
		t.Fatalf("windows = %+v", windows)
	}
	now = time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	models = `{"label":"Gemini 4 Argon (High)","modelId":"gemini-4-argon-high","quotaInfo":{"remainingFraction":0.2,"resetTime":"2026-11-10T06:00:00Z"}},
			{"label":"Claude Opus 5.5 (High)","modelId":"claude-opus-5-5-high","quotaInfo":{"remainingFraction":0.9,"resetTime":"2026-11-10T05:00:00Z"}}`
	if err := collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	latest, _ := st.Latest(context.Background(), Antigravity, now)
	got = map[string]Window{}
	for _, w := range latest {
		got[w.Key] = w
	}
	if len(latest) != 3 || got["gemini-4-argon:10080"].LastPercent != 80 || got["claude-opus-5-5:300"].LastPercent != 10 ||
		got["claude-opus-5-5:10080"].LastPercent != 100 {
		t.Fatalf("latest = %+v", latest)
	}
	if err := os.RemoveAll(daemon); err != nil {
		t.Fatal(err)
	}
	if err := collector.Collect(context.Background()); err != ErrAntigravityClosed {
		t.Fatalf("closed error = %v", err)
	}
}

func TestClaudePlanLabel(t *testing.T) {
	for _, tc := range []struct {
		display, orgType, rateTier, seat string
		max, pro                         bool
		want                             string
	}{
		{"Max 20x", "claude_max", "", "", true, false, "Max 20x"},
		{"", "claude_max", "default_claude_max_20x", "", true, false, "Max 20x"},
		{"", "claude_max", "default_claude_max_5x", "", true, false, "Max 5x"},
		{"", "claude_pro", "default_claude_ai", "", false, true, "Pro"},
		{"", "claude_team", "default_claude_max_5x", "team_tier_1", false, false, "Team · Premium"},
		{"", "claude_team", "", "team_standard", false, false, "Team · Standard"},
		{"", "claude_enterprise", "", "enterprise_usage_based", false, false, "Enterprise · Usage-based"},
		{"", "", "", "", true, false, "Max"},
		{"", "", "default_claude_zero", "", false, false, ""},
	} {
		if got := claudePlanLabel(tc.display, tc.orgType, tc.rateTier, tc.seat, tc.max, tc.pro); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc, got, tc.want)
		}
	}
}
