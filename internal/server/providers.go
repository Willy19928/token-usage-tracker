package server

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"time"

	"tokenusage/internal/ledger"
	"tokenusage/internal/model"
	"tokenusage/internal/pricing"
	"tokenusage/internal/store"
)

// selection is the dashboard's provider switch: all, codex, claude or
// antigravity. Codex lives in the main store; the others are ledgers.
type selection struct {
	codex   bool
	ledgers []*ledger.Provider
}

func (sel selection) codexOnly() bool { return sel.codex && len(sel.ledgers) == 0 }

func (s *Server) selection(query url.Values) (selection, error) {
	switch value := query.Get("provider"); value {
	case "", "all":
		return selection{codex: true, ledgers: s.Ledgers}, nil
	case "codex":
		return selection{codex: true}, nil
	default:
		for _, p := range s.Ledgers {
			if p.Name == value {
				return selection{ledgers: []*ledger.Provider{p}}, nil
			}
		}
		return selection{}, fmt.Errorf("provider: 不支援的來源 %q", value)
	}
}

var breakdownDimensions = map[string]bool{
	"model": true, "source": true, "agent_type": true, "project": true, "thread": true, "confidence": true, "provenance": true,
}

func (s *Server) location() *time.Location { return s.Store.Location() }

func (s *Server) ledgerPrice(builder *pricing.Builder, p *ledger.Provider, entries []ledger.Entry) error {
	machine := s.Store.Machine().ID
	for _, e := range entries {
		cost := pricing.ExternalCost{
			Priced: e.Price.Priced, RegularNano: e.Price.RegularNano, CachedNano: e.Price.CachedNano,
			CacheWriteNano: e.Price.CacheWriteNano, OutputNano: e.Price.OutputNano,
			ReasonKind: e.Price.ReasonKind, Reason: e.Price.Reason,
		}
		if err := builder.AddExternal(p.Event(e, machine, s.location()), cost); err != nil {
			return err
		}
	}
	return nil
}

// combinedRevision changes whenever any selected source changes.
func (s *Server) combinedRevision(ctx context.Context, sel selection) (uint64, error) {
	var revision uint64
	if sel.codex {
		value, err := s.Store.DataRevision(ctx)
		if err != nil {
			return 0, err
		}
		revision = value
	}
	for _, p := range sel.ledgers {
		revision = revision*1_000_003 + p.Revision()
	}
	return revision, nil
}

func mergeSummary(a, b model.Summary) model.Summary {
	out := a
	out.Usage = a.Usage.Add(b.Usage)
	out.Modes.Regular = a.Modes.Regular.Add(b.Modes.Regular)
	out.Modes.Fast = a.Modes.Fast.Add(b.Modes.Fast)
	out.Modes.Unknown = a.Modes.Unknown.Add(b.Modes.Unknown)
	out.GrandTotal = out.Usage.Total
	out.EventCount += b.EventCount
	out.SessionCount += b.SessionCount
	if out.FirstEvent.IsZero() || (!b.FirstEvent.IsZero() && b.FirstEvent.Before(out.FirstEvent)) {
		out.FirstEvent = b.FirstEvent
	}
	if b.LastEvent.After(out.LastEvent) {
		out.LastEvent = b.LastEvent
	}
	out.CoverageIncomplete = a.CoverageIncomplete || b.CoverageIncomplete
	return out
}

func mergePoints(a, b []model.Point) []model.Point {
	byKey := make(map[string]int, len(a))
	out := append([]model.Point{}, a...)
	for index, point := range out {
		byKey[point.Date] = index
	}
	for _, point := range b {
		if index, ok := byKey[point.Date]; ok {
			current := &out[index]
			current.Usage = current.Usage.Add(point.Usage)
			current.Modes.Regular = current.Modes.Regular.Add(point.Modes.Regular)
			current.Modes.Fast = current.Modes.Fast.Add(point.Modes.Fast)
			current.Modes.Unknown = current.Modes.Unknown.Add(point.Modes.Unknown)
			continue
		}
		byKey[point.Date] = len(out)
		out = append(out, point)
	}
	ledger.SortPoints(out)
	return out
}

func mergeBreakdown(a, b []model.BreakdownItem) []model.BreakdownItem {
	byKey := make(map[string]int, len(a))
	out := append([]model.BreakdownItem{}, a...)
	for index, item := range out {
		byKey[item.Key] = index
	}
	for _, item := range b {
		if index, ok := byKey[item.Key]; ok {
			current := &out[index]
			current.Usage = current.Usage.Add(item.Usage)
			current.Modes.Regular = current.Modes.Regular.Add(item.Modes.Regular)
			current.Modes.Fast = current.Modes.Fast.Add(item.Modes.Fast)
			current.Modes.Unknown = current.Modes.Unknown.Add(item.Modes.Unknown)
			current.Events += item.Events
			current.Sessions += item.Sessions
			continue
		}
		byKey[item.Key] = len(out)
		out = append(out, item)
	}
	return out
}

func sortBreakdown(items []model.BreakdownItem, limit int) []model.BreakdownItem {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Usage.Total == items[j].Usage.Total {
			return items[i].Key < items[j].Key
		}
		return items[i].Usage.Total > items[j].Usage.Total
	})
	if limit <= 0 || limit > 500 {
		limit = 25
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func mergeStrings(values ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range values {
		for _, value := range list {
			if value != "" && !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return lowerLess(out[i], out[j]) })
	return out
}

func lowerLess(a, b string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return ca < cb
		}
	}
	return len(a) < len(b)
}

// mixedSessionRows returns every matching session across the selected sources,
// most recent first, plus which ledger (if any) owns each session.
func (s *Server) mixedSessionRows(ctx context.Context, data *store.Store, sel selection, filter model.Filter) ([]store.SessionRow, map[string]*ledger.Provider, error) {
	var rows []store.SessionRow
	owners := map[string]*ledger.Provider{}
	if sel.codex {
		codexRows, err := data.Sessions(ctx, filter, -1, 0)
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, codexRows...)
	}
	for _, p := range sel.ledgers {
		for _, row := range p.Sessions(p.Select(filter, s.location())) {
			owners[row.SessionID] = p
			rows = append(rows, row)
		}
	}
	ledger.SortSessions(rows)
	return rows, owners, nil
}

// mixedSessionEstimates prices each session with its own source's rules.
func (s *Server) mixedSessionEstimates(ctx context.Context, data *store.Store, filter model.Filter, ids []string, owners map[string]*ledger.Provider) (map[string]pricing.Estimate, error) {
	overrides, err := s.pricingOverrides()
	if err != nil {
		return nil, err
	}
	builders := make(map[string]*pricing.Builder, len(ids))
	var codexIDs []string
	ledgerIDs := map[*ledger.Provider]map[string]bool{}
	for _, id := range ids {
		builder, err := pricing.NewBuilderForBasis(overrides, filter.CostBasis, data.Location())
		if err != nil {
			return nil, err
		}
		builders[id] = builder
		if owner := owners[id]; owner != nil {
			if ledgerIDs[owner] == nil {
				ledgerIDs[owner] = map[string]bool{}
			}
			ledgerIDs[owner][id] = true
		} else {
			codexIDs = append(codexIDs, id)
		}
	}
	for start := 0; start < len(codexIDs); start += 500 {
		chunk := codexIDs[start:min(start+500, len(codexIDs))]
		if err := data.WalkSessionPricingAggregates(ctx, filter, chunk, func(event model.UsageEvent) error {
			if builder := builders[event.SessionID]; builder != nil {
				return builder.Add(event)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for owner, wanted := range ledgerIDs {
		bySession := map[string][]ledger.Entry{}
		for _, e := range owner.Select(filter, s.location()) {
			if wanted[e.Session] {
				bySession[e.Session] = append(bySession[e.Session], e)
			}
		}
		for id, entries := range bySession {
			if err := s.ledgerPrice(builders[id], owner, entries); err != nil {
				return nil, err
			}
		}
	}
	out := make(map[string]pricing.Estimate, len(builders))
	for id, builder := range builders {
		out[id] = builder.Report().Summary
	}
	return out, nil
}

type ledgerStatus struct {
	Name         string          `json:"name"`
	Label        string          `json:"label"`
	DatabasePath string          `json:"database_path"`
	Busy         bool            `json:"busy"`
	LastError    string          `json:"last_error,omitempty"`
	EventCount   int             `json:"event_count"`
	Scan         ledger.ScanInfo `json:"scan,omitempty"`
}

func (s *Server) ledgerStatuses(ctx context.Context) []ledgerStatus {
	out := make([]ledgerStatus, 0, len(s.Ledgers))
	for _, p := range s.Ledgers {
		out = append(out, ledgerStatus{
			Name: p.Name, Label: p.Label, DatabasePath: p.Path(), Busy: p.Busy(), LastError: p.LastError(),
			EventCount: len(p.Entries()), Scan: p.ScanInfo(ctx),
		})
	}
	return out
}

// ScanLedgers imports new Claude Code and Antigravity usage.
func (s *Server) ScanLedgers(ctx context.Context) error {
	var first error
	for _, p := range s.Ledgers {
		if err := p.Scan(ctx); err != nil && first == nil {
			first = fmt.Errorf("%s: %w", p.Label, err)
		}
	}
	return first
}
