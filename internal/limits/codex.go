package limits

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tokenusage/internal/usage"
)

// codexAccountsSince marks when account tracking began. Windows observed
// before it have no recorded account and are shown as history of their plan.
const codexAccountsSince = "codex_accounts_since"

// Token-count records are small; anything larger is a prompt or tool output.
const maxLimitRecord = 4 << 20

// CodexCollector reads the rate-limit snapshots Codex writes with each token
// count and the account signed in to each Codex home.
type CodexCollector struct {
	Store *Store
	Homes func() ([]string, error)
	Now   func() time.Time
}

type codexRecord struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits *struct {
			LimitID   string       `json:"limit_id"`
			PlanType  string       `json:"plan_type"`
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type codexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

func (c *CodexCollector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Collect records the current accounts and any snapshots written since the
// previous pass.
func (c *CodexCollector) Collect(ctx context.Context) error {
	homes, err := c.Homes()
	if err != nil {
		return err
	}
	now := c.now().UTC()
	since, err := c.Store.meta(ctx, codexAccountsSince)
	if err != nil {
		return err
	}
	if since == "" {
		since = now.Format(time.RFC3339)
		if err := c.Store.setMeta(ctx, codexAccountsSince, since); err != nil {
			return err
		}
	}
	tracked, _ := time.Parse(time.RFC3339, since)
	for _, home := range homes {
		if account, ok := codexAccount(home); ok {
			if err := c.Store.RecordAccount(ctx, Codex, home, account, now); err != nil {
				return err
			}
		}
		for _, path := range usage.WalkRollouts(home) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := c.collectFile(ctx, home, path, tracked); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (c *CodexCollector) collectFile(ctx context.Context, home, path string, tracked time.Time) error {
	info, err := usage.StatRollout(path)
	if err != nil {
		return err
	}
	offset, size, err := c.Store.cursor(ctx, path)
	if err != nil {
		return err
	}
	if info.Size() == size && offset == size {
		return nil
	}
	if info.Size() < offset {
		// Rewritten or truncated: readings are idempotent, so start over.
		offset = 0
	}
	reader, err := usage.OpenRolloutAt(path, offset)
	if err != nil {
		return err
	}
	defer reader.Close()
	buffered := bufio.NewReaderSize(reader, 256<<10)
	committed := offset
	var minutes []usageMinute
	for {
		line, consumed, complete, err := readRecord(buffered)
		if !complete {
			break
		}
		committed += consumed
		if line != nil {
			minute, ok, err := c.observe(ctx, home, line, tracked)
			if err != nil {
				return err
			}
			if ok {
				minutes = append(minutes, minute)
			}
		}
		if err != nil {
			break
		}
	}
	if err := c.Store.recordUsageAccounts(ctx, home, fillUsageAccounts(minutes)); err != nil {
		return err
	}
	return c.Store.putCursor(ctx, path, committed, info.Size())
}

// readRecord returns the next complete line when it may hold a rate-limit
// snapshot, skipping large unrelated records without keeping them in memory.
func readRecord(reader *bufio.Reader) (line []byte, consumed int64, complete bool, err error) {
	var kept []byte
	relevant, decided := false, false
	for {
		fragment, readErr := reader.ReadSlice('\n')
		consumed += int64(len(fragment))
		if !decided && len(fragment) > 0 {
			relevant = bytes.Contains(fragment, []byte(`"rate_limits"`)) || errors.Is(readErr, bufio.ErrBufferFull)
			decided = !errors.Is(readErr, bufio.ErrBufferFull)
		}
		if relevant && len(kept)+len(fragment) <= maxLimitRecord {
			kept = append(kept, fragment...)
		} else {
			relevant = false
		}
		switch {
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case readErr == nil:
			if relevant && bytes.Contains(kept, []byte(`"rate_limits"`)) {
				return kept, consumed, true, nil
			}
			return nil, consumed, true, nil
		default:
			// A final line without a newline may still be being written.
			return nil, 0, false, readErr
		}
	}
}

// codexSnapshot is the rate-limit snapshot written with one token count.
type codexSnapshot struct {
	at          time.Time
	plan        string
	limitID     string
	anchorKey   string
	anchorReset time.Time
	anchorUsed  float64
	windows     []*codexWindow
}

func parseCodexSnapshot(line []byte) (codexSnapshot, bool) {
	var record codexRecord
	if json.Unmarshal(line, &record) != nil || record.Payload.Type != "token_count" || record.Payload.RateLimits == nil {
		return codexSnapshot{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, record.Timestamp)
	if err != nil {
		return codexSnapshot{}, false
	}
	limits := record.Payload.RateLimits
	// Every account has its own long window, so a snapshot's account is
	// identified by the reset of its longest window, plus the plan.
	var anchor *codexWindow
	for _, window := range []*codexWindow{limits.Primary, limits.Secondary} {
		if window != nil && window.ResetsAt != 0 && (anchor == nil || window.WindowMinutes > anchor.WindowMinutes) {
			anchor = window
		}
	}
	if anchor == nil {
		return codexSnapshot{}, false
	}
	limitID := firstNonEmpty(limits.LimitID, "codex")
	return codexSnapshot{
		at: at, plan: limits.PlanType, limitID: limitID,
		anchorKey: limitID + ":" + strconv.Itoa(anchor.WindowMinutes), anchorReset: time.Unix(anchor.ResetsAt, 0), anchorUsed: anchor.UsedPercent,
		windows: []*codexWindow{limits.Primary, limits.Secondary},
	}, true
}

// observe records a snapshot's windows and returns the account that used the
// token count on the same line, or no account when that is not known yet.
func (c *CodexCollector) observe(ctx context.Context, home string, line []byte, tracked time.Time) (usageMinute, bool, error) {
	snapshot, ok := parseCodexSnapshot(line)
	if !ok {
		return usageMinute{}, false, nil
	}
	account := ""
	var err error
	if !snapshot.at.Before(tracked) {
		if account, err = c.Store.AccountAt(ctx, Codex, home, snapshot.at); err != nil {
			return usageMinute{}, false, err
		}
	}
	used := usageMinute{minute: snapshot.at.Unix() / 60 * 60, account: account}
	if account == "" {
		// Before tracking, the account is unknown but still distinct: name it
		// after its window until a tracked reading of that window claims it.
		if account, err = c.Store.historyAccount(ctx, snapshot.plan, snapshot.anchorKey, snapshot.anchorReset); err != nil {
			return usageMinute{}, false, err
		}
		// A window read at 0% keeps sliding and is never recorded, so it names
		// no account; the conversation's other snapshots do.
		if snapshot.anchorUsed > 0 {
			used.account = account
		}
	} else if err := c.Store.adoptHistory(ctx, account, snapshot.anchorKey, snapshot.anchorReset); err != nil {
		return usageMinute{}, false, err
	}
	for _, window := range snapshot.windows {
		if window == nil || window.ResetsAt == 0 || window.WindowMinutes == 0 {
			continue
		}
		if err := c.Store.Observe(ctx, Observation{
			Provider: Codex, Account: account, Plan: snapshot.plan,
			Key: snapshot.limitID + ":" + strconv.Itoa(window.WindowMinutes), Label: snapshot.limitID, WindowMinutes: window.WindowMinutes,
			UsedPercent: window.UsedPercent, ResetsAt: time.Unix(window.ResetsAt, 0), ObservedAt: snapshot.at,
		}); err != nil {
			return usageMinute{}, false, err
		}
	}
	return used, true, nil
}

// usageMinute is the account that used Codex in one minute (Unix seconds).
type usageMinute struct {
	minute  int64
	account string
}

// fillUsageAccounts gives the snapshots of one rollout whose account is not
// known the account of the nearest known one before them, else after them:
// a conversation runs under one sign-in.
func fillUsageAccounts(minutes []usageMinute) map[int64]string {
	out := map[int64]string{}
	last := ""
	for _, m := range minutes {
		if m.account != "" {
			last = m.account
		}
		if last != "" {
			out[m.minute] = last
		}
	}
	next := ""
	for i := len(minutes) - 1; i >= 0; i-- {
		if minutes[i].account != "" {
			next = minutes[i].account
		}
		if _, ok := out[minutes[i].minute]; !ok && next != "" {
			out[minutes[i].minute] = next
		}
	}
	return out
}

// codexAccount reads the signed-in identity from a Codex home's auth.json.
// Only the identity claims are decoded; no token is kept.
func codexAccount(home string) (Account, bool) {
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return Account{}, false
	}
	var auth struct {
		Tokens struct {
			IDToken   string `json:"id_token"`
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return Account{}, false
	}
	parts := strings.Split(auth.Tokens.IDToken, ".")
	if len(parts) < 2 {
		return Account{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Account{}, false
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
			UserID    string `json:"chatgpt_user_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return Account{}, false
	}
	accountID := firstNonEmpty(claims.Auth.AccountID, auth.Tokens.AccountID)
	id := accountID
	if claims.Auth.UserID != "" {
		// Members of one team workspace share its account id.
		id += "/" + claims.Auth.UserID
	}
	if id == "" {
		id = claims.Email
	}
	return Account{ID: id, Email: claims.Email, Plan: claims.Auth.PlanType}, id != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
