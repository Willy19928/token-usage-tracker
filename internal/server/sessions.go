package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tokenusage/internal/model"
	"tokenusage/internal/pricing"
	"tokenusage/internal/store"
)

type sessionQueryKey struct {
	Mode, CostBasis            string
	Revision                   uint64
	SinceUnix, UntilUnix       int64
	SinceDate, UntilDate       string
	Model, Source, AgentType   string
	Project, SessionID, Search string
	Confidence                 string
	Limit, Offset              int
	Compact                    bool
}

type sessionEstimateCacheKey struct {
	Query           sessionQueryKey
	PricingRevision [32]byte
}

type sessionResponseItem struct {
	store.SessionRow
	Estimate pricing.Estimate `json:"estimate"`
}

type sessionEstimateResponseItem struct {
	SessionID string           `json:"session_id"`
	Estimate  pricing.Estimate `json:"estimate"`
}

func timeCacheKey(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func makeSessionQueryKey(revision uint64, filter model.Filter, limit, offset int, compact bool) sessionQueryKey {
	return sessionQueryKey{
		Revision: revision, Mode: filter.Mode, CostBasis: filter.CostBasis, SinceUnix: timeCacheKey(filter.Since), UntilUnix: timeCacheKey(filter.Until),
		SinceDate: filter.SinceDate, UntilDate: filter.UntilDate,
		Model: filter.Model, Source: filter.Source, AgentType: filter.AgentType,
		Project: filter.Project, SessionID: filter.SessionID, Search: filter.Search,
		Confidence: filter.Confidence, Limit: limit, Offset: offset, Compact: compact,
	}
}

func sessionRequestParameters(r *http.Request, locations ...*time.Location) (model.Filter, int, int, bool, error) {
	loc := time.Local
	if len(locations) > 0 {
		loc = locations[0]
	}
	filter, err := parseFilterIn(r.URL.Query(), loc)
	if err != nil {
		return model.Filter{}, 0, 0, false, err
	}
	limit := parseInt(r.URL.Query().Get("limit"), 100)
	offset := parseInt(r.URL.Query().Get("offset"), 0)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	compact := r.URL.Query().Get("compact") == "1" || strings.EqualFold(r.URL.Query().Get("compact"), "true")
	return filter, limit, offset, compact, nil
}

func (s *Server) cachedSessionRows(ctx context.Context, filter model.Filter, limit, offset int, compact bool, snapshots ...*store.Store) (sessionQueryKey, []store.SessionRow, bool, error) {
	data := s.Store
	if len(snapshots) > 0 {
		data = snapshots[0]
	}
	revision, err := data.DataRevision(ctx)
	if err != nil {
		return sessionQueryKey{}, nil, false, err
	}
	key := makeSessionQueryKey(revision, filter, limit, offset, compact)
	if items, ok := s.sessionRowsCache.get(key); ok {
		return key, items, true, nil
	}
	items, err := data.Sessions(ctx, filter, limit, offset)
	if err != nil {
		return key, nil, false, err
	}
	if compact {
		for index := range items {
			items[index].Title = compactText(items[index].Title, 240)
		}
	}
	s.sessionRowsCache.put(key, items)
	return key, items, false, nil
}

func (s *Server) cachedSessionEstimates(ctx context.Context, queryKey sessionQueryKey, filter model.Filter, items []store.SessionRow, snapshots ...*store.Store) ([]sessionEstimateResponseItem, bool, error) {
	data := s.Store
	if len(snapshots) > 0 {
		data = snapshots[0]
	}
	overrides, err := s.pricingOverrides()
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(overrides)
	if err != nil {
		return nil, false, err
	}
	cacheKey := sessionEstimateCacheKey{Query: queryKey, PricingRevision: sha256.Sum256(encoded)}
	if estimates, ok := s.sessionEstimateCache.get(cacheKey); ok {
		return estimates, true, nil
	}
	builders := make(map[string]*pricing.Builder, len(items))
	sessionIDs := make([]string, 0, len(items))
	for _, item := range items {
		builder, buildErr := pricing.NewBuilderForBasis(overrides, filter.CostBasis, data.Location())
		if buildErr != nil {
			return nil, false, buildErr
		}
		builders[item.SessionID] = builder
		sessionIDs = append(sessionIDs, item.SessionID)
	}
	if err := data.WalkSessionPricingAggregates(ctx, filter, sessionIDs, func(event model.UsageEvent) error {
		builder := builders[event.SessionID]
		if builder == nil {
			return nil
		}
		return builder.Add(event)
	}); err != nil {
		return nil, false, err
	}
	estimates := make([]sessionEstimateResponseItem, 0, len(items))
	for _, item := range items {
		estimates = append(estimates, sessionEstimateResponseItem{
			SessionID: item.SessionID,
			Estimate:  builders[item.SessionID].Report().Summary,
		})
	}
	s.sessionEstimateCache.put(cacheKey, estimates)
	return estimates, false, nil
}

func includeSessionEstimates(r *http.Request) bool {
	value := strings.TrimSpace(r.URL.Query().Get("include_estimate"))
	return value != "0" && !strings.EqualFold(value, "false")
}

func timingMetric(name string, elapsed time.Duration, hit bool) string {
	state := "miss"
	if hit {
		state = "hit"
	}
	return fmt.Sprintf(`%s;dur=%.2f;desc="%s"`, name, float64(elapsed.Microseconds())/1000, state)
}

// Both endpoints return the snapshot revision so clients can reject a delayed
// estimate response from a different ledger than the visible rows.
func (s *Server) sessionPage(ctx context.Context, filter model.Filter, limit, offset int, compact, estimates bool) (sessionQueryKey, []store.SessionRow, []sessionEstimateResponseItem, string, error) {
	var key sessionQueryKey
	var items []store.SessionRow
	var costs []sessionEstimateResponseItem
	var timings []string
	err := s.Store.ReadSnapshot(ctx, func(data *store.Store) error {
		started := time.Now()
		var hit bool
		var err error
		key, items, hit, err = s.cachedSessionRows(ctx, filter, limit, offset, compact, data)
		if err != nil {
			return err
		}
		timings = append(timings, timingMetric("sessions", time.Since(started), hit))
		if estimates {
			started = time.Now()
			costs, hit, err = s.cachedSessionEstimates(ctx, key, filter, items, data)
			timings = append(timings, timingMetric("pricing", time.Since(started), hit))
		}
		return err
	})
	return key, items, costs, strings.Join(timings, ", "), err
}

// handleMixedSessionPage serves the session list when Claude Code or
// Antigravity ledgers are selected; each session is priced by its own source.
func (s *Server) handleMixedSessionPage(w http.ResponseWriter, r *http.Request, sel selection, filter model.Filter, limit, offset int, compact, onlyEstimates bool) {
	var items []store.SessionRow
	var estimates map[string]pricing.Estimate
	var revision uint64
	err := s.Store.ReadSnapshot(r.Context(), func(data *store.Store) error {
		var err error
		if revision, err = s.combinedRevision(r.Context(), sel); err != nil {
			return err
		}
		rows, owners, err := s.mixedSessionRows(r.Context(), data, sel, filter)
		if err != nil {
			return err
		}
		end := min(offset+limit, len(rows))
		if offset < end {
			items = rows[offset:end]
		}
		if compact {
			for index := range items {
				items[index].Title = compactText(items[index].Title, 240)
			}
		}
		if onlyEstimates || includeSessionEstimates(r) {
			ids := make([]string, 0, len(items))
			for _, item := range items {
				ids = append(ids, item.SessionID)
			}
			estimates, err = s.mixedSessionEstimates(r.Context(), data, filter, ids, owners)
		}
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if items == nil {
		items = []store.SessionRow{}
	}
	var response any = items
	if onlyEstimates {
		list := make([]sessionEstimateResponseItem, 0, len(items))
		for _, item := range items {
			list = append(list, sessionEstimateResponseItem{SessionID: item.SessionID, Estimate: estimates[item.SessionID]})
		}
		response = list
	} else if includeSessionEstimates(r) {
		merged := make([]sessionResponseItem, 0, len(items))
		for _, item := range items {
			merged = append(merged, sessionResponseItem{SessionRow: item, Estimate: estimates[item.SessionID]})
		}
		response = merged
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": response, "data_revision": revision})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	s.handleSessionPage(w, r, false)
}
func (s *Server) handleSessionEstimates(w http.ResponseWriter, r *http.Request) {
	s.handleSessionPage(w, r, true)
}
func (s *Server) handleSessionPage(w http.ResponseWriter, r *http.Request, onlyEstimates bool) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	filter, limit, offset, compact, err := sessionRequestParameters(r, s.Store.Location())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sel, err := s.selection(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !sel.codexOnly() {
		s.handleMixedSessionPage(w, r, sel, filter, limit, offset, compact, onlyEstimates)
		return
	}
	key, items, estimates, timing, err := s.sessionPage(r.Context(), filter, limit, offset, compact, onlyEstimates || includeSessionEstimates(r))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Server-Timing", timing)
	var response any = items
	if onlyEstimates {
		response = estimates
	} else if includeSessionEstimates(r) {
		bySession := make(map[string]pricing.Estimate, len(estimates))
		for _, item := range estimates {
			bySession[item.SessionID] = item.Estimate
		}
		merged := make([]sessionResponseItem, 0, len(items))
		for _, item := range items {
			merged = append(merged, sessionResponseItem{SessionRow: item, Estimate: bySession[item.SessionID]})
		}
		response = merged
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": response, "data_revision": key.Revision})
}
