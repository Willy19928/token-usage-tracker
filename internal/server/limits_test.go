package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tokenusage/internal/limits"
	"tokenusage/internal/model"
	"tokenusage/internal/store"
)

func TestLimitsAPIFoldsSharedQuotasAndTogglesClaude(t *testing.T) {
	st, err := limits.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	for _, o := range []limits.Observation{
		{Provider: limits.Antigravity, Account: "a", Key: "claude-opus-5-5", Label: "Claude Opus 5.5", WindowMinutes: 300, UsedPercent: 40, ResetsAt: reset},
		{Provider: limits.Antigravity, Account: "a", Key: "gpt-oss-120b", Label: "GPT-OSS 120B", WindowMinutes: 300, UsedPercent: 40, ResetsAt: reset.Add(20 * time.Second)},
		{Provider: limits.Antigravity, Account: "a", Key: "gemini-4-argon", Label: "Gemini 4 Argon", WindowMinutes: 10080, UsedPercent: 70, ResetsAt: reset.Add(-200 * time.Hour)},
		// Untouched: current only, never a window or a chart point.
		{Provider: limits.Antigravity, Account: "a", Key: "gemini-3-flash", Label: "Gemini 3 Flash", WindowMinutes: 300, UsedPercent: 0, ResetsAt: reset.Add(time.Hour)},
	} {
		o.ObservedAt = time.Now()
		if err := st.Observe(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	enabled := false
	s := &Server{
		Limits:            &limits.Manager{Store: st},
		ClaudeUsageAPI:    func() bool { return enabled },
		SetClaudeUsageAPI: func(value bool) error { enabled = value; return nil },
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits?provider=antigravity", nil))
	var body limitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(body.Windows) != 2 || strings.Join(body.Windows[0].Labels, ",") != "Claude Opus 5.5,GPT-OSS 120B" ||
		!body.Windows[0].Active || body.Windows[1].Active || body.Status.State != limits.StateIdle {
		t.Fatalf("windows = %+v", body)
	}
	if !body.Windows[1].StartsAt.Equal(body.Windows[1].ResetsAt.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("weekly start = %v", body.Windows[1].StartsAt)
	}
	// The reset weekly pool stays at 0% with no window in progress until the
	// next reading; the untouched pool is there too.
	if len(body.Current) != 3 || strings.Join(body.Current[0].Labels, ",") != "Gemini 4 Argon" || body.Current[0].LastPercent != 0 ||
		body.Current[0].Active || body.Current[0].Tokens != nil || strings.Join(body.Current[2].Labels, ",") != "Gemini 3 Flash" {
		t.Fatalf("current = %+v", body.Current)
	}
	// Untouched summary groups report the same sliding reset but stay apart.
	for _, key := range []string{"gemini-5h", "3p-5h"} {
		if err := st.Observe(ctx, limits.Observation{Provider: limits.Antigravity, Account: "b", Key: key, Label: key,
			WindowMinutes: 300, UsedPercent: 0, ResetsAt: reset, ObservedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits?provider=antigravity", nil))
	body = limitsResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Current) != 5 {
		t.Fatalf("summary groups were folded: %s", w.Body.String())
	}
	// A removed account leaves current until a newer reading, and can be returned.
	hide := func(account string, hidden bool) {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:43189/api/v1/limits/hidden",
			strings.NewReader(fmt.Sprintf(`{"provider":"antigravity","account":%q,"hidden":%t}`, account, hidden))))
		if w.Code != 200 {
			t.Fatalf("hide %s: %d %s", account, w.Code, w.Body.String())
		}
	}
	current := func() (map[string]int, map[string]bool) {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits?provider=antigravity", nil))
		var body limitsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		counts, hidden := map[string]int{}, map[string]bool{}
		for _, window := range body.Current {
			counts[window.Account]++
		}
		for _, account := range body.Accounts {
			hidden[account.ID] = account.Hidden
		}
		return counts, hidden
	}
	if err := st.RecordAccount(ctx, limits.Antigravity, "", limits.Account{ID: "b", Email: "b@example.test"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	hide("b", true)
	if counts, hidden := current(); counts["b"] != 0 || counts["a"] != 3 || !hidden["b"] {
		t.Fatalf("after hiding b: %v %v", counts, hidden)
	}
	hide("b", false)
	if counts, hidden := current(); counts["b"] != 2 || hidden["b"] {
		t.Fatalf("after showing b: %v %v", counts, hidden)
	}
	hide("b", true)
	if err := st.Observe(ctx, limits.Observation{Provider: limits.Antigravity, Account: "b", Key: "gemini-5h", Label: "gemini-5h",
		WindowMinutes: 300, UsedPercent: 3, ResetsAt: reset, ObservedAt: time.Now().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if counts, hidden := current(); counts["b"] != 2 || hidden["b"] {
		t.Fatalf("a newer reading did not return b: %v %v", counts, hidden)
	}
	// Unidentified accounts never appear in current.
	if err := st.Observe(ctx, limits.Observation{Provider: limits.Codex, Account: limits.HistoryPrefix + "team:1", Key: "codex:10080",
		WindowMinutes: 10080, UsedPercent: 100, ResetsAt: reset, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits?provider=codex", nil))
	if !strings.Contains(w.Body.String(), `"current":[]`) || !strings.Contains(w.Body.String(), `"unknown":true`) {
		t.Fatalf("codex response = %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits/series?days=30", nil))
	var series struct {
		Series []limitSeries `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &series); err != nil || len(series.Series) != 4 || len(series.Series[0].Points) != 1 {
		t.Fatalf("series = %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		method, path, body, origin string
		code                       int
	}{
		{"GET", "/api/v1/limits?provider=nope", "", "", 400},
		{"POST", "/api/v1/limits/claude", `{"enabled":true}`, "https://evil.example", 403},
		{"POST", "/api/v1/limits/claude", `{}`, "", 400},
		{"GET", "/api/v1/limits/claude", "", "", 405},
		{"POST", "/api/v1/limits/claude", `{"enabled":true}`, "", 200},
	} {
		r := httptest.NewRequest(tc.method, "http://127.0.0.1:43189"+tc.path, strings.NewReader(tc.body))
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if !enabled {
		t.Fatal("claude polling was not enabled")
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits?provider=claude", nil))
	if !strings.Contains(w.Body.String(), `"claude_enabled":true`) {
		t.Fatalf("claude response = %s", w.Body.String())
	}
	// Claude's and Codex's limits are each their own quota, even when two read
	// the same and reset together.
	for _, o := range []limits.Observation{
		{Provider: limits.Claude, Account: "c", Key: "seven_day", WindowMinutes: 10080, UsedPercent: 12, ResetsAt: reset},
		{Provider: limits.Claude, Account: "c", Key: "weekly_scoped:fable", WindowMinutes: 10080, UsedPercent: 12, ResetsAt: reset},
		{Provider: limits.Codex, Account: "d", Key: "codex:10080", WindowMinutes: 10080, UsedPercent: 30, ResetsAt: reset},
		{Provider: limits.Codex, Account: "d", Key: "codex_other:10080", WindowMinutes: 10080, UsedPercent: 30, ResetsAt: reset},
	} {
		o.ObservedAt = time.Now()
		if err := st.Observe(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, provider := range []string{limits.Claude, limits.Codex} {
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits?provider="+provider, nil))
		body = limitsResponse{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Current) != 2 ||
			len(body.Current[0].Keys) != 1 || len(body.Current[1].Keys) != 1 {
			t.Fatalf("%s limits were folded: %s", provider, w.Body.String())
		}
		for _, window := range body.Windows {
			if len(window.Keys) != 1 {
				t.Fatalf("%s windows were folded: %s", provider, w.Body.String())
			}
		}
	}
}

func TestLimitWindowsCarryTokensAndCost(t *testing.T) {
	data, err := store.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	ctx := context.Background()
	now := time.Now()
	for _, tier := range []string{"default", "priority"} {
		if _, err := data.InsertEvent(ctx, model.UsageEvent{ID: "e" + tier, SessionID: "s", TurnID: "t" + tier, Timestamp: now.Add(-time.Hour),
			Model: "gpt-6-astra", Usage: model.TokenUsage{Input: 1000000, Total: 1000000}, ServiceMode: model.ModeFromTier(tier, "jsonl_turn_context"),
			Provenance: model.ProvenanceSessionJSONL}, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Another account's usage in the same window is not this account's.
	if _, err := data.InsertEvent(ctx, model.UsageEvent{ID: "other", SessionID: "s2", TurnID: "t", Timestamp: now.Add(-30 * time.Minute),
		Model: "gpt-6-astra", Usage: model.TokenUsage{Input: 500000, Total: 500000}, Provenance: model.ProvenanceSessionJSONL}, ""); err != nil {
		t.Fatal(err)
	}
	limitsDir := t.TempDir()
	st, err := limits.Open(limitsDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The Codex collector records which account each minute's usage belongs to.
	raw, err := sql.Open("sqlite", filepath.Join(limitsDir, "limits.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO codex_usage_accounts(home,minute,account) VALUES('h',?,'a'),('h',?,'b')`,
		now.Add(-time.Hour).Unix()/60*60, now.Add(-30*time.Minute).Unix()/60*60); err != nil {
		t.Fatal(err)
	}
	if err := st.Observe(ctx, limits.Observation{Provider: limits.Codex, Account: "a", Key: "codex:300", WindowMinutes: 300,
		UsedPercent: 40, ResetsAt: now.Add(3 * time.Hour), ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: data, Limits: &limits.Manager{Store: st}}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits?provider=codex", nil))
	var body limitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Current) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Priced like the dashboard: Fast mode at its credit multiplier.
	current := body.Current[0]
	if current.Tokens == nil || *current.Tokens != 2000000 || current.Estimate == nil || current.Estimate.USD != "35.000000000" {
		t.Fatalf("current = %s", w.Body.String())
	}
}

func TestLimitSeriesCustomRange(t *testing.T) {
	st, err := limits.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.Local) }
	for _, d := range []int{3, 10, 20} {
		if err := st.Observe(ctx, limits.Observation{Provider: limits.Claude, Account: "a", Key: "seven_day", WindowMinutes: 10080,
			UsedPercent: float64(d), ResetsAt: day(d).Add(48 * time.Hour), ObservedAt: day(d)}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{Limits: &limits.Manager{Store: st}}
	get := func(query string) (int, string) {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:43189/api/v1/limits/series?"+query, nil))
		return w.Code, w.Body.String()
	}
	code, body := get("since=2026-10-05&until=2026-10-15")
	var series struct {
		Series []limitSeries `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &series); err != nil || code != 200 || len(series.Series) != 1 ||
		len(series.Series[0].Points) != 1 || series.Series[0].Points[0].Percent != 10 {
		t.Fatalf("range = %d %s", code, body)
	}
	for _, query := range []string{"since=2026-10-15&until=2026-10-05", "since=2024-01-01&until=2026-10-05", "since=nope", "days=200"} {
		if code, body := get(query); code != 400 {
			t.Errorf("%s: %d %s", query, code, body)
		}
	}
}

func TestLimitModelMatcherCountsTheLimitsModels(t *testing.T) {
	models := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1", "gpt-oss-120b", "gemini-3.8-flash", "gemini-4-argon"}
	for _, tc := range []struct {
		provider string
		keys     []string
		want     string // matched models joined by ","; "*" for all; "-" when not attributable
	}{
		{limits.Claude, []string{"five_hour"}, "*"},
		{limits.Claude, []string{"seven_day_opus"}, "claude-opus-5-5"},
		{limits.Claude, []string{"weekly_scoped:fable"}, "claude-fable-5-1"},
		{limits.Claude, []string{"seven_day_cowork"}, "-"},
		{limits.Antigravity, []string{"3p-weekly"}, "claude-opus-5-5,claude-sonnet-5-5,claude-fable-5-1,gpt-oss-120b"},
		{limits.Antigravity, []string{"gemini-5h"}, "gemini-3.8-flash"},
		{limits.Antigravity, []string{"gemini-argon-wtu-weekly"}, "gemini-4-argon"},
		{limits.Antigravity, []string{"claude-opus-5-5:300", "gpt-oss-120b:300"}, "claude-opus-5-5,gpt-oss-120b"},
		{limits.Codex, []string{"codex:300"}, "*"},
	} {
		match, ok := limitModelMatcher(tc.provider, tc.keys)
		got := "-"
		switch {
		case ok && match == nil:
			got = "*"
		case ok:
			var matched []string
			for _, model := range models {
				if match(model) {
					matched = append(matched, model)
				}
			}
			got = strings.Join(matched, ",")
		}
		if got != tc.want {
			t.Errorf("%s %v = %q, want %q", tc.provider, tc.keys, got, tc.want)
		}
	}
}
