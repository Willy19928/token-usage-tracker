package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"tokenusage/internal/store"
)

// planOf returns the ChatGPT plan named by a token_count's rate limits: the
// plan, "" when the rate limits name none (an API request), and ok false when
// the record carries no rate limits.
func planOf(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false
	}
	var limits struct {
		PlanType *string `json:"plan_type"`
	}
	if json.Unmarshal(raw, &limits) != nil {
		return "", false
	}
	if limits.PlanType == nil {
		return "", true
	}
	return *limits.PlanType, true
}

func (s *Scanner) recordPlan(ctx context.Context, session, turn string, raw json.RawMessage) error {
	plan, ok := planOf(raw)
	if !ok || session == "" || turn == "" {
		return nil
	}
	key := session + "\x00" + turn + "\x00" + plan
	if s.lastPlan == key {
		return nil
	}
	if err := s.Store.PutTurnPlans(ctx, []store.TurnPlan{{SessionID: session, TurnID: turn, Plan: plan}}); err != nil {
		return err
	}
	s.lastPlan = key
	return nil
}

// backfillPlans records the turn plans of the part of a file read before
// plans were kept.
func (s *Scanner) backfillPlans(ctx context.Context, path string, cursor store.FileCursor) error {
	checked, err := s.Store.PlanFileChecked(ctx, path)
	if err != nil || checked || cursor.SessionID == "" {
		return err
	}
	if cursor.Offset > 0 {
		f, err := openRollout(path)
		if err != nil {
			return err
		}
		defer f.Close()
		reader := bufio.NewReaderSize(io.LimitReader(f, cursor.Offset), 64<<10)
		plans := map[string]string{}
		var order []string
		turn := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			record, n, complete, large, err := readSelectiveRecord(reader, s.MaxRelevantRecord)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if n == 0 {
				break
			}
			if !complete || large {
				continue
			}
			var env envelope
			if json.Unmarshal(record, &env) != nil {
				continue
			}
			switch env.Type {
			case "turn_context":
				var p turnContextPayload
				if json.Unmarshal(env.Payload, &p) == nil {
					turn = firstNonEmpty(p.TurnID, turn)
				}
			case "event_msg":
				var p eventPayload
				if json.Unmarshal(env.Payload, &p) != nil || p.Type != "token_count" {
					continue
				}
				turn = firstNonEmpty(p.TurnID, turn)
				plan, ok := planOf(p.RateLimits)
				if !ok || turn == "" {
					continue
				}
				old, seen := plans[turn]
				if !seen {
					order = append(order, turn)
				}
				if !seen || old == "" {
					plans[turn] = plan
				}
			}
		}
		items := make([]store.TurnPlan, 0, len(order))
		for _, turn := range order {
			items = append(items, store.TurnPlan{SessionID: cursor.SessionID, TurnID: turn, Plan: plans[turn]})
		}
		if err := s.Store.PutTurnPlans(ctx, items); err != nil {
			return err
		}
	}
	return s.Store.MarkPlanFileChecked(ctx, path)
}
