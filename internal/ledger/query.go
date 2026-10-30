package ledger

import (
	"sort"
	"strings"
	"time"

	"tokenusage/internal/model"
	"tokenusage/internal/store"
)

// The query helpers answer the dashboard APIs from the in-memory ledger with
// the same filter semantics as the Codex store (local-date bounds are
// inclusive/exclusive dates, absolute bounds are [since, until)).

func confidenceOf(e Entry) string {
	if e.Precision == "recorded" {
		return model.ConfidenceExact
	}
	return "estimated"
}

// Select returns the entries matching filter, oldest first.
func (p *Provider) Select(filter model.Filter, loc *time.Location) []Entry {
	entries := p.Entries()
	var sessions map[string]bool
	if filter.Search != "" {
		needle := strings.ToLower(filter.Search)
		sessions = map[string]bool{}
		for _, e := range entries {
			if strings.Contains(e.Session, needle) || strings.Contains(strings.ToLower(e.Model), needle) ||
				strings.Contains(strings.ToLower(p.Label), needle) {
				sessions[e.Session] = true
			}
		}
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if filter.SinceDate != "" {
			if e.Timestamp.In(loc).Format("2006-01-02") < filter.SinceDate {
				continue
			}
		} else if !filter.Since.IsZero() && e.Timestamp.Before(filter.Since) {
			continue
		}
		if filter.UntilDate != "" {
			if e.Timestamp.In(loc).Format("2006-01-02") >= filter.UntilDate {
				continue
			}
		} else if !filter.Until.IsZero() && !e.Timestamp.Before(filter.Until) {
			continue
		}
		if (filter.Model != "" && e.Model != filter.Model) ||
			(filter.Source != "" && filter.Source != p.Label) ||
			(filter.AgentType != "" && filter.AgentType != "main") ||
			filter.Project != "" ||
			(filter.SessionID != "" && e.Session != filter.SessionID) ||
			(filter.Confidence != "" && confidenceOf(e) != filter.Confidence) {
			continue
		}
		switch filter.Mode {
		case "regular":
			if e.Mode == model.ModeFast {
				continue
			}
		case "fast":
			if e.Mode != model.ModeFast {
				continue
			}
		case "unknown":
			if e.Mode != model.ModeUnknown {
				continue
			}
		}
		if sessions != nil && !sessions[e.Session] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Event converts an entry to the shared export/pricing event shape.
func (p *Provider) Event(e Entry, machineID string, loc *time.Location) model.UsageEvent {
	local := e.Timestamp.In(loc)
	return model.UsageEvent{
		ServiceMode: model.ServiceMode{ServiceMode: e.Mode, ModeSource: p.Name + "_ledger"},
		ID:          e.ID, Timestamp: e.Timestamp, LocalDate: local.Format("2006-01-02"), LocalHour: local.Format("15"),
		ObservedAt: e.Timestamp, MachineID: machineID, SessionID: e.Session, Model: e.Model,
		Source: p.Label, AgentType: "main", Usage: e.Usage, Provenance: e.TokenSource, Confidence: confidenceOf(e),
	}
}

func Summary(entries []Entry) model.Summary {
	var out model.Summary
	sessions := map[string]bool{}
	for _, e := range entries {
		out.Usage = out.Usage.Add(e.Usage)
		out.Modes.Add(e.Usage, e.Mode)
		out.EventCount++
		sessions[e.Session] = true
		if out.FirstEvent.IsZero() || e.Timestamp.Before(out.FirstEvent) {
			out.FirstEvent = e.Timestamp
		}
		if e.Timestamp.After(out.LastEvent) {
			out.LastEvent = e.Timestamp
		}
	}
	out.SessionCount = int64(len(sessions))
	out.GrandTotal = out.Usage.Total
	out.Modes.Complete(out.Usage)
	return out
}

func Timeseries(entries []Entry, bucket string, loc *time.Location) []model.Point {
	points := map[string]*model.Point{}
	for _, e := range entries {
		local := e.Timestamp.In(loc)
		var key string
		var at time.Time
		if bucket == "hour" {
			start := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, loc)
			key, at = start.Format("2006-01-02T15"), start.UTC()
		} else {
			key = local.Format("2006-01-02")
			at, _ = time.ParseInLocation("2006-01-02", key, time.UTC)
		}
		point := points[key]
		if point == nil {
			point = &model.Point{Time: at, Date: key}
			points[key] = point
		}
		point.Usage = point.Usage.Add(e.Usage)
		point.Modes.Add(e.Usage, e.Mode)
	}
	out := make([]model.Point, 0, len(points))
	for _, point := range points {
		point.Modes.Complete(point.Usage)
		out = append(out, *point)
	}
	SortPoints(out)
	return out
}

func SortPoints(points []model.Point) {
	sort.Slice(points, func(i, j int) bool { return points[i].Time.Before(points[j].Time) })
}

func (p *Provider) Breakdown(entries []Entry, dimension string) []model.BreakdownItem {
	items := map[string]*model.BreakdownItem{}
	sessions := map[string]map[string]bool{}
	for _, e := range entries {
		key := "未知"
		switch dimension {
		case "model":
			key = e.Model
		case "source":
			key = p.Label
		case "agent_type":
			key = "main"
		case "confidence":
			key = confidenceOf(e)
		case "provenance":
			key = e.TokenSource
		}
		if key == "" {
			key = "未知"
		}
		item := items[key]
		if item == nil {
			item = &model.BreakdownItem{Key: key}
			items[key] = item
			sessions[key] = map[string]bool{}
		}
		item.Usage = item.Usage.Add(e.Usage)
		item.Modes.Add(e.Usage, e.Mode)
		item.Events++
		sessions[key][e.Session] = true
	}
	out := make([]model.BreakdownItem, 0, len(items))
	for key, item := range items {
		item.Sessions = int64(len(sessions[key]))
		item.Modes.Complete(item.Usage)
		out = append(out, *item)
	}
	return out
}

// Sessions groups entries per conversation. Ledgers keep no titles or project
// paths (by design they never store prompt content), so titles are synthetic.
func (p *Provider) Sessions(entries []Entry) []store.SessionRow {
	rows := map[string]*store.SessionRow{}
	modelTokens := map[string]map[string]int64{}
	for _, e := range entries {
		row := rows[e.Session]
		if row == nil {
			row = &store.SessionRow{SessionInfo: model.SessionInfo{
				SessionID: e.Session, Title: p.Label + " · " + e.Session[:min(8, len(e.Session))],
				Source: p.Label, AgentType: "main", CreatedAt: e.Timestamp,
			}, Confidence: model.ConfidenceExact}
			rows[e.Session] = row
			modelTokens[e.Session] = map[string]int64{}
		}
		row.Usage = row.Usage.Add(e.Usage)
		row.Modes.Add(e.Usage, e.Mode)
		row.EventCount++
		row.TokensUsed += e.Usage.Total
		if e.Timestamp.After(row.LastUsage) {
			row.LastUsage, row.UpdatedAt = e.Timestamp, e.Timestamp
		}
		if confidenceOf(e) != model.ConfidenceExact {
			row.Confidence = confidenceOf(e)
		}
		modelTokens[e.Session][e.Model] += e.Usage.Total
	}
	out := make([]store.SessionRow, 0, len(rows))
	for id, row := range rows {
		best, bestTokens := "", int64(-1)
		for name, tokens := range modelTokens[id] {
			if tokens > bestTokens || (tokens == bestTokens && name < best) {
				best, bestTokens = name, tokens
			}
		}
		row.Model = best
		row.Modes.Complete(row.Usage)
		out = append(out, *row)
	}
	SortSessions(out)
	return out
}

// SortSessions matches the Codex store order: most recent usage first.
func SortSessions(rows []store.SessionRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].LastUsage.Equal(rows[j].LastUsage) {
			return rows[i].SessionID < rows[j].SessionID
		}
		return rows[i].LastUsage.After(rows[j].LastUsage)
	})
}
