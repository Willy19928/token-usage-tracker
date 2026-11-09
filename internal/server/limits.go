package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"tokenusage/internal/ledger"
	"tokenusage/internal/limits"
	"tokenusage/internal/model"
	"tokenusage/internal/pricing"
)

// limitWindow is one window on the page. Antigravity reports several models
// that share one quota, so identical readings are folded into one row.
type limitWindow struct {
	Account       string    `json:"account"`
	Plan          string    `json:"plan"`
	Keys          []string  `json:"keys"`
	Labels        []string  `json:"labels"`
	WindowMinutes int       `json:"window_minutes"`
	StartsAt      time.Time `json:"starts_at,omitzero"`
	ResetsAt      time.Time `json:"resets_at"`
	EndedAt       time.Time `json:"ended_at,omitzero"` // an early reset ended it before ResetsAt
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	MaxPercent    float64   `json:"max_percent"`
	LastPercent   float64   `json:"last_percent"`
	Active        bool      `json:"active"`
	Tokens        *int64    `json:"tokens,omitempty"`
	// The API-equivalent cost of the same usage, priced like the dashboard.
	Estimate *pricing.Estimate `json:"estimate,omitempty"`
}

type limitsResponse struct {
	Provider      string           `json:"provider"`
	Status        limits.Status    `json:"status"`
	ClaudeEnabled *bool            `json:"claude_enabled,omitempty"`
	Accounts      []limits.Account `json:"accounts"`
	Current       []limitWindow    `json:"current"`
	Windows       []limitWindow    `json:"windows"`
}

const maxLimitWindows = 300

func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request) {
	if s.Limits == nil {
		http.Error(w, "額度紀錄無法使用", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/api/v1/limits/claude":
		s.handleClaudeLimitsSetting(w, r)
		return
	case "/api/v1/limits/series":
		s.handleLimitSeries(w, r)
		return
	case "/api/v1/limits/hidden":
		s.handleLimitHidden(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provider := r.URL.Query().Get("provider")
	switch provider {
	case limits.Codex, limits.Claude, limits.Antigravity:
	default:
		http.Error(w, "provider: 不支援的來源 "+provider, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	windows, err := s.Limits.Store.Windows(ctx, provider, maxLimitWindows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	accounts, err := s.Limits.Store.Accounts(ctx, provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	unknown, err := s.Limits.Store.HistoryAccounts(ctx, provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	latest, err := s.Limits.Store.Latest(ctx, provider, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Current is about signed-in accounts; unidentified ones stay in history.
	kept := latest[:0]
	for _, window := range latest {
		if !strings.HasPrefix(window.Account, limits.HistoryPrefix) {
			kept = append(kept, window)
		}
	}
	latest = kept
	hidden, err := s.Limits.Store.HiddenAccounts(ctx, provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range accounts {
		accounts[i].Hidden = hidden[accounts[i].ID]
	}
	out := limitsResponse{Provider: provider, Status: s.Limits.Status(provider), Accounts: append(accounts, unknown...),
		Current: foldLimitWindows(latest, now), Windows: foldLimitWindows(windows, now)}
	if out.Accounts == nil {
		out.Accounts = []limits.Account{}
	}
	if out.Current == nil {
		out.Current = []limitWindow{}
	}
	if provider == limits.Claude && s.ClaudeUsageAPI != nil {
		enabled := s.ClaudeUsageAPI()
		out.ClaudeEnabled = &enabled
	}
	// Tokens used so far in each window and their API-equivalent cost:
	// current windows up to now, past windows up to their reset.
	overrides, err := s.pricingOverrides()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Ended windows only change when the usage data or the prices do, so
	// their totals are kept for that revision.
	revision, cacheable := s.limitUsageRevision(ctx, provider, overrides)
	cached, _ := s.limitUsageCache.get(revision)
	totals := map[string]limitUsageTotal{}
	for n, list := range [][]limitWindow{out.Current, out.Windows} {
		for i := range list {
			window := &list[i]
			// A current limit past its reset has no window in progress yet.
			if window.StartsAt.IsZero() || (n == 0 && !window.Active) {
				continue
			}
			match, ok := limitModelMatcher(provider, window.Keys)
			if !ok {
				continue
			}
			end := window.ResetsAt
			if !window.EndedAt.IsZero() {
				end = minTime(end, window.EndedAt)
			}
			until := minTime(end, now)
			key := fmt.Sprintf("%s|%s|%d|%d", window.Account, strings.Join(window.Keys, ","), window.StartsAt.Unix(), until.Unix())
			ended := cacheable && !end.After(now)
			if usage, ok := cached[key]; ok && ended {
				totals[key] = usage
				window.Tokens, window.Estimate = &usage.tokens, &usage.estimate
				continue
			}
			if tokens, estimate, ok := s.limitUsage(ctx, provider, window.Account, window.StartsAt, until, match, overrides); ok {
				window.Tokens, window.Estimate = &tokens, &estimate
				if ended {
					totals[key] = limitUsageTotal{tokens: tokens, estimate: estimate}
				}
			}
		}
	}
	if cacheable {
		s.limitUsageCache.put(revision, totals)
	}
	writeJSON(w, http.StatusOK, out)
}

type limitPoint struct {
	Time     time.Time `json:"t"`
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resets_at"`
}

type limitSeries struct {
	Provider      string       `json:"provider"`
	Account       string       `json:"account"`
	Email         string       `json:"email"`
	Plan          string       `json:"plan"`
	Key           string       `json:"key"`
	Label         string       `json:"label"`
	WindowMinutes int          `json:"window_minutes"`
	Points        []limitPoint `json:"points"`
}

// handleLimitSeries returns every limit's readings over the last days, for
// the overview chart. A change of resets_at starts a new window.
func (s *Server) handleLimitSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	since, until, err := s.limitSeriesRange(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	readings, err := s.Limits.Store.Readings(ctx, since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	emails := map[string]string{}
	for _, provider := range []string{limits.Codex, limits.Claude, limits.Antigravity} {
		accounts, err := s.Limits.Store.Accounts(ctx, provider)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range accounts {
			emails[provider+"|"+a.ID] = a.Email
		}
	}
	series := []limitSeries{}
	index := map[string]int{}
	for _, reading := range readings {
		id := reading.Provider + "|" + reading.Account + "|" + reading.Key
		i, ok := index[id]
		if !ok {
			i = len(series)
			index[id] = i
			series = append(series, limitSeries{Provider: reading.Provider, Account: reading.Account,
				Email: emails[reading.Provider+"|"+reading.Account], Key: reading.Key})
		}
		item := &series[i]
		item.Plan, item.Label, item.WindowMinutes = reading.Plan, reading.Label, reading.WindowMinutes
		item.Points = append(item.Points, limitPoint{Time: reading.ObservedAt, Percent: reading.UsedPercent, ResetsAt: reading.ResetsAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"since": since, "until": until, "series": series})
}

// The longest range the chart draws; a year of readings is still small.
const maxLimitSeriesRange = 366 * 24 * time.Hour

// limitSeriesRange reads either days=N (the preset buttons) or since/until in
// the formats and accounting timezone the rest of the dashboard uses.
func (s *Server) limitSeriesRange(query url.Values) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	if query.Get("since") == "" && query.Get("until") == "" {
		days := 7
		if value := query.Get("days"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > 90 {
				return time.Time{}, time.Time{}, fmt.Errorf("days 只能是 1 到 90")
			}
			days = parsed
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), now, nil
	}
	loc := time.Local
	if s.Store != nil {
		loc = s.location()
	}
	since, err := parseSinceIn(query.Get("since"), loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("since: %w", err)
	}
	until := now
	if value := query.Get("until"); value != "" {
		if until, err = parseAbsoluteTimeIn(value, loc); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("until: %w", err)
		}
	}
	if !since.Before(until) {
		return time.Time{}, time.Time{}, fmt.Errorf("until: 結束時間必須晚於開始時間 / End time must be after start time")
	}
	if until.Sub(since) > maxLimitSeriesRange {
		return time.Time{}, time.Time{}, fmt.Errorf("區間最長 366 天")
	}
	return since.UTC(), until.UTC(), nil
}

// handleLimitHidden removes a signed-in account from the current view, or
// returns it; a newer reading of the account returns it on its own.
func (s *Server) handleLimitHidden(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Account  string `json:"account"`
		Hidden   *bool  `json:"hidden"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Hidden == nil ||
		body.Account == "" || strings.HasPrefix(body.Account, limits.HistoryPrefix) {
		http.Error(w, "無效請求內容", http.StatusBadRequest)
		return
	}
	switch body.Provider {
	case limits.Codex, limits.Claude, limits.Antigravity:
	default:
		http.Error(w, "provider: 不支援的來源 "+body.Provider, http.StatusBadRequest)
		return
	}
	var err error
	if *body.Hidden {
		err = s.Limits.Store.HideAccount(r.Context(), body.Provider, body.Account, time.Now())
	} else {
		err = s.Limits.Store.ShowAccount(r.Context(), body.Provider, body.Account)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": body.Provider, "account": body.Account, "hidden": *body.Hidden})
}

func (s *Server) handleClaudeLimitsSetting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || s.SetClaudeUsageAPI == nil {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || body.Enabled == nil {
		http.Error(w, "無效請求內容", http.StatusBadRequest)
		return
	}
	if err := s.SetClaudeUsageAPI(*body.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Limits.Wake(limits.Claude)
	writeJSON(w, http.StatusOK, map[string]bool{"claude_enabled": *body.Enabled})
}

// foldLimitWindows merges Antigravity's per-model readings of one shared quota
// (same account, reset and readings), and marks windows that have not reset yet.
func foldLimitWindows(windows []limits.Window, now time.Time) []limitWindow {
	var out []limitWindow
	// Shared quotas report resets a few seconds apart, so compare them within
	// the store's tolerance rather than by fixed time buckets.
	same := func(f limitWindow, w limits.Window) bool {
		gap := f.ResetsAt.Sub(w.ResetsAt)
		return f.Account == w.Account && f.WindowMinutes == w.WindowMinutes && gap <= 2*time.Minute && gap >= -2*time.Minute && f.EndedAt.Equal(w.EndedAt) &&
			formatPercent(f.MaxPercent) == formatPercent(w.MaxPercent) && formatPercent(f.LastPercent) == formatPercent(w.LastPercent)
	}
	for _, window := range windows {
		folded := false
		for i := range out {
			if perModelQuota(window) && perModelQuota(limits.Window{Provider: window.Provider, Key: out[i].Keys[0]}) && same(out[i], window) {
				out[i].Keys = append(out[i].Keys, window.Key)
				out[i].Labels = append(out[i].Labels, window.Label)
				folded = true
				break
			}
		}
		if folded {
			continue
		}
		entry := limitWindow{
			Account: window.Account, Plan: window.Plan, Keys: []string{window.Key}, Labels: []string{window.Label},
			WindowMinutes: window.WindowMinutes, ResetsAt: window.ResetsAt, FirstSeen: window.FirstSeen,
			LastSeen: window.LastSeen, MaxPercent: window.MaxPercent, LastPercent: window.LastPercent,
			EndedAt: window.EndedAt, Active: window.ResetsAt.After(now) && (window.EndedAt.IsZero() || window.EndedAt.After(now)),
		}
		if !window.StartedAt.IsZero() {
			entry.StartsAt = window.StartedAt
		} else if window.WindowMinutes > 0 {
			entry.StartsAt = window.ResetsAt.Add(-time.Duration(window.WindowMinutes) * time.Minute)
		}
		out = append(out, entry)
	}
	for i := range out {
		sort.Strings(out[i].Labels)
	}
	return out
}

// perModelQuota says a window is one model's view of a quota it may share.
// Antigravity's summary buckets, Claude's limits and Codex's windows are each
// their own quota, and two of them never merge, even when both are untouched
// and report the same sliding reset.
func perModelQuota(w limits.Window) bool {
	return w.Provider == limits.Antigravity && !strings.HasSuffix(w.Key, "-5h") && !strings.HasSuffix(w.Key, "-weekly")
}

func formatPercent(value float64) string {
	return strconv.FormatFloat(math.Round(value*100)/100, 'f', -1, 64)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// limitModelMatcher says which models a limit counts. A nil matcher counts
// every model; ok is false for limits on a surface rather than on models,
// whose usage the ledgers cannot separate.
func limitModelMatcher(provider string, keys []string) (match func(string) bool, ok bool) {
	var parts []func(string) bool
	for _, key := range keys {
		key = strings.ToLower(key)
		switch provider {
		case limits.Claude:
			switch {
			case key == "five_hour" || key == "seven_day":
				return nil, true
			case key == "seven_day_opus":
				parts = append(parts, containsAny("opus"))
			case key == "seven_day_sonnet":
				parts = append(parts, containsAny("sonnet"))
			case strings.HasPrefix(key, "weekly_scoped:") || strings.HasPrefix(key, "session_scoped:"):
				id := key[strings.Index(key, ":")+1:]
				if id == "" || strings.Contains(id, "cowork") || strings.Contains(id, "app") {
					return nil, false
				}
				parts = append(parts, containsAny(id))
			default:
				return nil, false
			}
		case limits.Antigravity:
			bucket, _, _ := strings.Cut(key, ":")
			switch {
			case strings.HasPrefix(bucket, "gemini-argon"):
				parts = append(parts, containsAny("argon"))
			case strings.HasPrefix(bucket, "gemini-weekly") || strings.HasPrefix(bucket, "gemini-5h"):
				parts = append(parts, func(model string) bool { return strings.Contains(model, "gemini") && !strings.Contains(model, "argon") })
			case strings.HasPrefix(bucket, "3p-"):
				parts = append(parts, containsAny("claude", "gpt"))
			default:
				// Older Antigravity names each model family itself.
				parts = append(parts, func(model string) bool { return strings.HasPrefix(model, bucket) })
			}
		default:
			return nil, true
		}
	}
	if len(parts) == 0 {
		return nil, true
	}
	return func(model string) bool {
		model = strings.ToLower(model)
		for _, part := range parts {
			if part(model) {
				return true
			}
		}
		return false
	}, true
}

func containsAny(words ...string) func(string) bool {
	return func(model string) bool {
		for _, word := range words {
			if strings.Contains(model, word) {
				return true
			}
		}
		return false
	}
}

type limitUsageTotal struct {
	tokens   int64
	estimate pricing.Estimate
}

// limitUsageRevision names the state of a provider's usage data and of the
// price overrides; a cached window total is valid only for the same one.
func (s *Server) limitUsageRevision(ctx context.Context, provider string, overrides map[string]pricing.Override) (string, bool) {
	if s.Store == nil {
		return "", false
	}
	sel := selection{codex: provider == limits.Codex}
	for _, p := range s.Ledgers {
		if p.Name == provider {
			sel.ledgers = append(sel.ledgers, p)
		}
	}
	revision, err := s.combinedRevision(ctx, sel)
	if err != nil {
		return "", false
	}
	prices, err := json.Marshal(overrides)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s:%d:%x", provider, revision, sha256.Sum256(prices)), true
}

// limitUsage totals the provider's usage within a window, across accounts,
// counting only the models match accepts when it is set, and prices it the
// way the dashboard's cost views do.
func (s *Server) limitUsage(ctx context.Context, provider, account string, since, until time.Time, match func(string) bool,
	overrides map[string]pricing.Override) (int64, pricing.Estimate, bool) {
	if s.Store == nil {
		return 0, pricing.Estimate{}, false
	}
	// The dashboard's cost views all count Fast mode at its credit multiplier.
	builder, err := pricing.NewBuilderForBasis(overrides, pricing.FastWeightedBasis, s.location())
	if err != nil {
		return 0, pricing.Estimate{}, false
	}
	if !until.After(since) {
		return 0, builder.Report().Summary, true
	}
	if provider == limits.Codex {
		// Each Codex token count is written with the snapshot that names its
		// account, so a window counts only the time its own account was in use.
		ranges, err := s.Limits.Store.CodexUsageRanges(ctx, account, since, until)
		if err != nil {
			return 0, pricing.Estimate{}, false
		}
		var total int64
		for _, span := range ranges {
			filter := model.Filter{Since: span.Since, Until: span.Until}
			summary, err := s.Store.Summary(ctx, filter)
			if err != nil || s.forEachPricingEvent(ctx, filter, builder.Add) != nil {
				return 0, pricing.Estimate{}, false
			}
			total += summary.GrandTotal
		}
		return total, builder.Report().Summary, true
	}
	filter := model.Filter{Since: since, Until: until}
	for _, p := range s.Ledgers {
		if p.Name == provider {
			var total int64
			var entries []ledger.Entry
			for _, entry := range p.Select(filter, s.location()) {
				if match == nil || match(entry.Model) {
					total += entry.Usage.Total
					entries = append(entries, entry)
				}
			}
			if s.ledgerPrice(builder, p, entries) != nil {
				return 0, pricing.Estimate{}, false
			}
			return total, builder.Report().Summary, true
		}
	}
	return 0, pricing.Estimate{}, false
}
