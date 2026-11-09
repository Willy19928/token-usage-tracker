// Package limits records the 5-hour and weekly usage windows each service
// reports: Codex in its rollouts, Claude through the official usage endpoint
// and Antigravity through its own local language server.
package limits

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Providers in the order the page shows them.
const (
	Codex       = "codex"
	Claude      = "claude"
	Antigravity = "antigravity"
)

// Reported reset times drift by a few seconds between snapshots of the same
// window, so observations this close together belong to one window.
const resetTolerance = 2 * time.Minute

// Observation is one reading of one limit.
type Observation struct {
	Provider      string
	Account       string
	Plan          string
	Key           string
	Label         string
	WindowMinutes int
	UsedPercent   float64
	ResetsAt      time.Time
	ObservedAt    time.Time
}

// Window is a limit period with the highest and latest usage observed in it.
type Window struct {
	Provider      string    `json:"provider"`
	Account       string    `json:"account"`
	Plan          string    `json:"plan"`
	Key           string    `json:"key"`
	Label         string    `json:"label"`
	WindowMinutes int       `json:"window_minutes"`
	ResetsAt      time.Time `json:"resets_at"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	MaxPercent    float64   `json:"max_percent"`
	LastPercent   float64   `json:"last_percent"`
	// StartedAt is set when the window began at an early reset rather than a
	// full window before ResetsAt; EndedAt when an early reset cut it short.
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
}

// Account is a signed-in identity seen for a provider. An unknown account is
// one recorded before account tracking began, known only by its plan and the
// reset of its longest window.
type Account struct {
	Provider       string    `json:"provider"`
	ID             string    `json:"id"`
	Email          string    `json:"email"`
	Plan           string    `json:"plan"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	Unknown        bool      `json:"unknown,omitempty"`
	AnchorResetsAt time.Time `json:"anchor_resets_at,omitzero"`
	// Hidden is set when the account was removed from the current view.
	Hidden bool `json:"hidden,omitempty"`
}

type Store struct {
	db   *sql.DB
	path string
}

const schema = `
CREATE TABLE IF NOT EXISTS windows (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	provider TEXT NOT NULL,
	account TEXT NOT NULL DEFAULT '',
	plan TEXT NOT NULL DEFAULT '',
	limit_key TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	window_minutes INTEGER NOT NULL DEFAULT 0,
	resets_at INTEGER NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	max_percent REAL NOT NULL DEFAULT 0,
	last_percent REAL NOT NULL DEFAULT 0,
	started_at INTEGER NOT NULL DEFAULT 0,
	ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS windows_lookup ON windows(provider, account, limit_key, resets_at);
CREATE TABLE IF NOT EXISTS readings (
	provider TEXT NOT NULL,
	account TEXT NOT NULL DEFAULT '',
	limit_key TEXT NOT NULL,
	plan TEXT NOT NULL DEFAULT '',
	label TEXT NOT NULL DEFAULT '',
	window_minutes INTEGER NOT NULL DEFAULT 0,
	used_percent REAL NOT NULL,
	resets_at INTEGER NOT NULL,
	observed_at INTEGER NOT NULL,
	PRIMARY KEY(provider, account, limit_key, observed_at)
);
CREATE INDEX IF NOT EXISTS readings_time ON readings(observed_at);
CREATE TABLE IF NOT EXISTS latest (
	provider TEXT NOT NULL,
	account TEXT NOT NULL DEFAULT '',
	limit_key TEXT NOT NULL,
	plan TEXT NOT NULL DEFAULT '',
	label TEXT NOT NULL DEFAULT '',
	window_minutes INTEGER NOT NULL DEFAULT 0,
	used_percent REAL NOT NULL DEFAULT 0,
	resets_at INTEGER NOT NULL,
	observed_at INTEGER NOT NULL,
	PRIMARY KEY(provider, account, limit_key)
);
CREATE TABLE IF NOT EXISTS accounts (
	provider TEXT NOT NULL,
	id TEXT NOT NULL,
	email TEXT NOT NULL DEFAULT '',
	plan TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	PRIMARY KEY(provider, id)
);
CREATE TABLE IF NOT EXISTS account_log (
	provider TEXT NOT NULL,
	scope TEXT NOT NULL DEFAULT '',
	observed_at INTEGER NOT NULL,
	account TEXT NOT NULL,
	PRIMARY KEY(provider, scope, observed_at)
);
CREATE TABLE IF NOT EXISTS file_cursors (
	path TEXT PRIMARY KEY,
	offset INTEGER NOT NULL,
	size INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS codex_usage_accounts (
	home TEXT NOT NULL,
	minute INTEGER NOT NULL,
	account TEXT NOT NULL,
	PRIMARY KEY(home, minute)
);
CREATE INDEX IF NOT EXISTS codex_usage_accounts_time ON codex_usage_accounts(minute);
CREATE TABLE IF NOT EXISTS hidden_accounts (
	provider TEXT NOT NULL,
	account TEXT NOT NULL,
	hidden_at INTEGER NOT NULL,
	PRIMARY KEY(provider, account)
);`

// Open opens or creates limits.sqlite in directory.
func Open(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "limits.sqlite")
	db, err := sql.Open("sqlite", sqliteURI(path, "_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

func sqliteURI(path, rawQuery string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	normalized := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && strings.HasPrefix(normalized, "//") {
		parts := strings.SplitN(strings.TrimPrefix(normalized, "//"), "/", 2)
		value := &url.URL{Scheme: "file", Host: parts[0], RawQuery: rawQuery}
		if len(parts) == 2 {
			value.Path = "/" + parts[1]
		}
		return value.String()
	}
	if runtime.GOOS == "windows" && len(normalized) >= 2 && normalized[1] == ':' {
		normalized = "/" + normalized
	}
	return (&url.URL{Scheme: "file", Path: normalized, RawQuery: rawQuery}).String()
}

// Observe folds a reading into its window, creating the window on first sight.
func (s *Store) Observe(ctx context.Context, o Observation) error {
	if o.ResetsAt.IsZero() || o.Key == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	resets, observed := o.ResetsAt.Unix(), o.ObservedAt.Unix()
	// The latest reading of every limit is kept, untouched ones included.
	if _, err := tx.ExecContext(ctx, `INSERT INTO latest(provider,account,limit_key,plan,label,window_minutes,used_percent,resets_at,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(provider,account,limit_key) DO UPDATE SET plan=excluded.plan,label=excluded.label,
		window_minutes=excluded.window_minutes,used_percent=excluded.used_percent,resets_at=excluded.resets_at,observed_at=excluded.observed_at
		WHERE excluded.observed_at>=latest.observed_at`,
		o.Provider, o.Account, o.Key, o.Plan, o.Label, o.WindowMinutes, o.UsedPercent, resets, observed); err != nil {
		return err
	}
	// An early reset (a reset card) zeroes Claude's limits but keeps their
	// reset times: a used window read at 0% ends there.
	tolerance := int64(resetTolerance.Seconds())
	if o.Provider == Claude {
		var open int64
		var lastPercent float64
		var lastSeen int64
		err := tx.QueryRowContext(ctx, `SELECT id,last_percent,last_seen FROM windows WHERE provider=? AND account=? AND limit_key=?
			AND resets_at BETWEEN ? AND ? AND ended_at=0 ORDER BY started_at DESC LIMIT 1`,
			o.Provider, o.Account, o.Key, resets-tolerance, resets+tolerance).Scan(&open, &lastPercent, &lastSeen)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && observed > lastSeen && o.UsedPercent <= 0 && lastPercent > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE windows SET ended_at=? WHERE id=?`, observed, open); err != nil {
				return err
			}
		}
	}
	// An untouched quota reports a reset one window from now, which slides
	// with every reading; a window only exists once something used it.
	if o.UsedPercent <= 0 {
		return tx.Commit()
	}
	// The chart series keeps a reading whenever it differs from the one
	// before it in time; repeated identical polls add nothing.
	var prevPercent float64
	var prevResets int64
	err = tx.QueryRowContext(ctx, `SELECT used_percent,resets_at FROM readings WHERE provider=? AND account=? AND limit_key=?
		AND observed_at<=? ORDER BY observed_at DESC LIMIT 1`, o.Provider, o.Account, o.Key, observed).Scan(&prevPercent, &prevResets)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (prevPercent != o.UsedPercent || abs64(prevResets-resets) > int64(resetTolerance.Seconds()))) {
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO readings(provider,account,limit_key,plan,label,window_minutes,used_percent,resets_at,observed_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, o.Provider, o.Account, o.Key, o.Plan, o.Label, o.WindowMinutes, o.UsedPercent, resets, observed)
	}
	if err != nil {
		return err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM windows WHERE provider=? AND account=? AND limit_key=?
		AND resets_at BETWEEN ? AND ? AND ended_at=0 ORDER BY ABS(resets_at-?) LIMIT 1`,
		o.Provider, o.Account, o.Key, resets-tolerance, resets+tolerance, resets).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// After an early reset the window goes on from where the last one ended.
		_, err = tx.ExecContext(ctx, `INSERT INTO windows(provider,account,plan,limit_key,label,window_minutes,
			resets_at,first_seen,last_seen,max_percent,last_percent,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,
			(SELECT COALESCE(MAX(ended_at),0) FROM windows WHERE provider=? AND account=? AND limit_key=? AND resets_at BETWEEN ? AND ?))`,
			o.Provider, o.Account, o.Plan, o.Key, o.Label, o.WindowMinutes, resets, observed, observed, o.UsedPercent, o.UsedPercent,
			o.Provider, o.Account, o.Key, resets-tolerance, resets+tolerance)
	case err == nil:
		// Readings can arrive out of order during a backfill: the latest
		// percentage is the one observed last, never simply the last written.
		_, err = tx.ExecContext(ctx, `UPDATE windows SET
			plan=CASE WHEN ?>=last_seen AND ?<>'' THEN ? ELSE plan END,
			label=CASE WHEN ?>=last_seen AND ?<>'' THEN ? ELSE label END,
			window_minutes=MAX(window_minutes, ?),
			last_percent=CASE WHEN ?>=last_seen THEN ? ELSE last_percent END,
			first_seen=MIN(first_seen, ?), last_seen=MAX(last_seen, ?),
			max_percent=MAX(max_percent, ?) WHERE id=?`,
			observed, o.Plan, o.Plan, observed, o.Label, o.Label, o.WindowMinutes,
			observed, o.UsedPercent, observed, observed, o.UsedPercent, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Windows lists a provider's windows, most recent reset first. Codex and
// Antigravity move the reset time on an early reset, so a window overlapped by
// a later window of the same limit ended when that one began.
func (s *Store) Windows(ctx context.Context, provider string, limit int) ([]Window, error) {
	tolerance := int64(resetTolerance.Seconds())
	rows, err := s.db.QueryContext(ctx, `SELECT w.provider,w.account,w.plan,w.limit_key,w.label,w.window_minutes,w.resets_at,
		w.first_seen,w.last_seen,w.max_percent,w.last_percent,w.started_at,
		CASE WHEN w.ended_at>0 THEN w.ended_at ELSE COALESCE((SELECT MIN(n.resets_at-n.window_minutes*60) FROM windows n
			WHERE n.provider=w.provider AND n.account=w.account AND n.limit_key=w.limit_key AND n.window_minutes>0
			AND n.resets_at>w.resets_at+? AND n.resets_at-n.window_minutes*60<w.resets_at-?),0) END
		FROM windows w WHERE w.provider=? ORDER BY w.resets_at DESC, w.started_at DESC, w.limit_key LIMIT ?`,
		tolerance, tolerance, provider, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		var w Window
		var resets, first, last, started, ended int64
		if err := rows.Scan(&w.Provider, &w.Account, &w.Plan, &w.Key, &w.Label, &w.WindowMinutes, &resets,
			&first, &last, &w.MaxPercent, &w.LastPercent, &started, &ended); err != nil {
			return nil, err
		}
		w.ResetsAt, w.FirstSeen, w.LastSeen = time.Unix(resets, 0).UTC(), time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		if started > 0 {
			w.StartedAt = time.Unix(started, 0).UTC()
		}
		if ended > 0 {
			w.EndedAt = time.Unix(ended, 0).UTC()
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Latest lists the most recent reading of each of a provider's limits, as
// windows whose maximum is that reading. A limit that has not reset stays; one
// past its reset stays at 0% while it was part of the account's most recent
// reading, and goes once a newer reading no longer has it. Accounts removed
// from the current view are left out until a reading newer than their removal.
func (s *Store) Latest(ctx context.Context, provider string, now time.Time) ([]Window, error) {
	rows, err := s.db.QueryContext(ctx, `WITH newest AS (SELECT account, MAX(observed_at) AS at FROM latest WHERE provider=? GROUP BY account)
		SELECT l.provider,l.account,l.plan,l.limit_key,l.label,l.window_minutes,l.resets_at,l.observed_at,l.used_percent
		FROM latest AS l JOIN newest AS n ON n.account=l.account
		WHERE l.provider=? AND (l.resets_at>? OR l.observed_at>=n.at-?) AND NOT EXISTS (SELECT 1 FROM hidden_accounts AS h
			WHERE h.provider=l.provider AND h.account=l.account AND h.hidden_at>=n.at)
		ORDER BY l.account, l.resets_at, l.limit_key`, provider, provider, now.Unix(), int64(resetTolerance.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		var w Window
		var resets, observed int64
		if err := rows.Scan(&w.Provider, &w.Account, &w.Plan, &w.Key, &w.Label, &w.WindowMinutes, &resets, &observed, &w.LastPercent); err != nil {
			return nil, err
		}
		w.ResetsAt = time.Unix(resets, 0).UTC()
		w.FirstSeen, w.LastSeen = time.Unix(observed, 0).UTC(), time.Unix(observed, 0).UTC()
		if resets <= now.Unix() {
			w.LastPercent = 0
		}
		w.MaxPercent = w.LastPercent
		out = append(out, w)
	}
	return out, rows.Err()
}

// HideAccount removes an account from the current view until a reading newer
// than at arrives; its windows and readings are kept.
func (s *Store) HideAccount(ctx context.Context, provider, account string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO hidden_accounts(provider,account,hidden_at) VALUES(?,?,?)
		ON CONFLICT(provider,account) DO UPDATE SET hidden_at=excluded.hidden_at`, provider, account, at.Unix())
	return err
}

// ShowAccount returns a removed account to the current view.
func (s *Store) ShowAccount(ctx context.Context, provider, account string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM hidden_accounts WHERE provider=? AND account=?`, provider, account)
	return err
}

// HiddenAccounts lists the accounts currently removed from the current view:
// those with no reading since their removal.
func (s *Store) HiddenAccounts(ctx context.Context, provider string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.account FROM hidden_accounts AS h WHERE h.provider=?
		AND h.hidden_at>=COALESCE((SELECT MAX(observed_at) FROM latest AS l WHERE l.provider=h.provider AND l.account=h.account),0)`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return nil, err
		}
		out[account] = true
	}
	return out, rows.Err()
}

// Reading is one point of a chart series.
type Reading struct {
	Provider      string
	Account       string
	Plan          string
	Key           string
	Label         string
	WindowMinutes int
	UsedPercent   float64
	ResetsAt      time.Time
	ObservedAt    time.Time
}

// Readings lists the readings observed from since through until, oldest first.
func (s *Store) Readings(ctx context.Context, since, until time.Time) ([]Reading, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider,account,plan,limit_key,label,window_minutes,used_percent,resets_at,observed_at
		FROM readings WHERE observed_at>=? AND observed_at<=? ORDER BY observed_at`, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reading
	for rows.Next() {
		var r Reading
		var resets, observed int64
		if err := rows.Scan(&r.Provider, &r.Account, &r.Plan, &r.Key, &r.Label, &r.WindowMinutes, &r.UsedPercent, &resets, &observed); err != nil {
			return nil, err
		}
		r.ResetsAt, r.ObservedAt = time.Unix(resets, 0).UTC(), time.Unix(observed, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// hasWindow reports whether a window with this key resets at reset.
func (s *Store) hasWindow(ctx context.Context, provider, account, key string, reset time.Time) (bool, error) {
	r := reset.Unix()
	tolerance := int64(resetTolerance.Seconds())
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM windows WHERE provider=? AND account=? AND limit_key=?
		AND resets_at BETWEEN ? AND ? LIMIT 1`, provider, account, key, r-tolerance, r+tolerance).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// HistoryPrefix marks an account recorded before account tracking began. It
// is named after its plan and the reset of its longest window.
const HistoryPrefix = "history:"

// historyAccount returns the unknown account whose window anchorKey resets at
// reset, reusing an existing one when the reset drifted by a few seconds.
func (s *Store) historyAccount(ctx context.Context, plan, anchorKey string, reset time.Time) (string, error) {
	var account string
	r := reset.Unix()
	tolerance := int64(resetTolerance.Seconds())
	err := s.db.QueryRowContext(ctx, `SELECT account FROM windows WHERE provider=? AND account LIKE ? AND limit_key=?
		AND resets_at BETWEEN ? AND ? ORDER BY ABS(resets_at-?) LIMIT 1`,
		Codex, HistoryPrefix+plan+":%", anchorKey, r-tolerance, r+tolerance, r).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return HistoryPrefix + plan + ":" + strconv.FormatInt(r, 10), nil
	}
	return account, err
}

// adoptHistory gives a known account the unknown history whose longest window
// is the one this account is in now.
func (s *Store) adoptHistory(ctx context.Context, account, anchorKey string, reset time.Time) error {
	r := reset.Unix()
	tolerance := int64(resetTolerance.Seconds())
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT account FROM windows WHERE provider=? AND account LIKE ? AND limit_key=?
		AND resets_at BETWEEN ? AND ?`, Codex, HistoryPrefix+"%", anchorKey, r-tolerance, r+tolerance)
	if err != nil {
		return err
	}
	var unknown []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		unknown = append(unknown, id)
	}
	rows.Close()
	if len(unknown) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range unknown {
		// The account's own latest reading wins over an older adopted one.
		if _, err := tx.ExecContext(ctx, `DELETE FROM latest WHERE provider=? AND account=? AND EXISTS (
			SELECT 1 FROM latest AS own WHERE own.provider=latest.provider AND own.account=? AND own.limit_key=latest.limit_key
			AND own.observed_at>=latest.observed_at)`, Codex, id, account); err != nil {
			return err
		}
		for _, stmt := range []string{
			`UPDATE OR REPLACE readings SET account=? WHERE provider=? AND account=?`,
			`UPDATE OR REPLACE latest SET account=? WHERE provider=? AND account=?`,
			`UPDATE windows SET account=? WHERE provider=? AND account=?`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, account, Codex, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE codex_usage_accounts SET account=? WHERE account=?`, account, id); err != nil {
			return err
		}
	}
	// The adopted window may already exist under the known account too.
	if _, err := tx.ExecContext(ctx, `UPDATE windows AS w SET
		first_seen=MIN(w.first_seen, d.first_seen), max_percent=MAX(w.max_percent, d.max_percent),
		last_percent=CASE WHEN d.last_seen>w.last_seen THEN d.last_percent ELSE w.last_percent END,
		last_seen=MAX(w.last_seen, d.last_seen)
		FROM windows AS d WHERE w.provider=? AND w.account=? AND d.provider=w.provider AND d.account=w.account
		AND d.limit_key=w.limit_key AND d.started_at=w.started_at AND d.id>w.id AND ABS(d.resets_at-w.resets_at)<=?`, Codex, account, tolerance); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM windows WHERE provider=? AND account=? AND EXISTS (
		SELECT 1 FROM windows AS keep WHERE keep.provider=windows.provider AND keep.account=windows.account
		AND keep.limit_key=windows.limit_key AND keep.started_at=windows.started_at AND keep.id<windows.id
		AND ABS(keep.resets_at-windows.resets_at)<=?)`,
		Codex, account, tolerance); err != nil {
		return err
	}
	return tx.Commit()
}

// recordUsageAccounts notes which account used Codex in each minute (Unix
// seconds) of a home.
func (s *Store) recordUsageAccounts(ctx context.Context, home string, minutes map[int64]string) error {
	if len(minutes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for minute, account := range minutes {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO codex_usage_accounts(home,minute,account) VALUES(?,?,?)`,
			home, minute, account); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TimeRange is a half-open span of time.
type TimeRange struct{ Since, Until time.Time }

// CodexUsageRanges returns the spans within [since, until) in which account
// was the one using Codex: runs of its minutes not interrupted by another
// account's.
func (s *Store) CodexUsageRanges(ctx context.Context, account string, since, until time.Time) ([]TimeRange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT minute, account FROM codex_usage_accounts WHERE minute>=? AND minute<?
		ORDER BY minute`, since.Unix()/60*60, until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeRange
	open := false
	for rows.Next() {
		var minute int64
		var owner string
		if err := rows.Scan(&minute, &owner); err != nil {
			return nil, err
		}
		if owner != account {
			open = false
			continue
		}
		start, end := time.Unix(minute, 0).UTC(), time.Unix(minute+60, 0).UTC()
		if open {
			out[len(out)-1].Until = end
		} else {
			out = append(out, TimeRange{Since: start, Until: end})
			open = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Since.Before(since) {
			out[i].Since = since
		}
		if out[i].Until.After(until) {
			out[i].Until = until
		}
	}
	return out, nil
}

func abs64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// RecordAccount notes that account was signed in at observedAt within scope
// (a Codex home, for example) and keeps its latest email and plan.
func (s *Store) RecordAccount(ctx context.Context, provider, scope string, account Account, observedAt time.Time) error {
	if account.ID == "" {
		return nil
	}
	at := observedAt.Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(provider,id,email,plan,first_seen,last_seen) VALUES(?,?,?,?,?,?)
		ON CONFLICT(provider,id) DO UPDATE SET email=CASE WHEN excluded.email<>'' THEN excluded.email ELSE email END,
		plan=CASE WHEN excluded.plan<>'' THEN excluded.plan ELSE plan END,
		first_seen=MIN(first_seen, excluded.first_seen), last_seen=MAX(last_seen, excluded.last_seen)`,
		provider, account.ID, account.Email, account.Plan, at, at); err != nil {
		return err
	}
	var current string
	err = tx.QueryRowContext(ctx, `SELECT account FROM account_log WHERE provider=? AND scope=? ORDER BY observed_at DESC LIMIT 1`,
		provider, scope).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != account.ID) {
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO account_log(provider,scope,observed_at,account) VALUES(?,?,?,?)`,
			provider, scope, at, account.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// AccountAt returns the account signed in within scope at t, or "" before any
// account was recorded there.
func (s *Store) AccountAt(ctx context.Context, provider, scope string, t time.Time) (string, error) {
	var account string
	err := s.db.QueryRowContext(ctx, `SELECT account FROM account_log WHERE provider=? AND scope=? AND observed_at<=?
		ORDER BY observed_at DESC LIMIT 1`, provider, scope, t.Unix()).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return account, err
}

// HistoryAccounts lists the unknown accounts that still own windows.
func (s *Store) HistoryAccounts(ctx context.Context, provider string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, MAX(plan), MIN(first_seen), MAX(last_seen) FROM windows
		WHERE provider=? AND account LIKE ? GROUP BY account ORDER BY MAX(last_seen) DESC`, provider, HistoryPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a := Account{Provider: provider, Unknown: true}
		var first, last int64
		if err := rows.Scan(&a.ID, &a.Plan, &first, &last); err != nil {
			return nil, err
		}
		a.FirstSeen, a.LastSeen = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		parts := strings.Split(strings.TrimPrefix(a.ID, HistoryPrefix), ":")
		if len(parts) == 2 {
			a.Plan = parts[0]
			if unix, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				a.AnchorResetsAt = time.Unix(unix, 0).UTC()
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Accounts lists the accounts seen for a provider.
func (s *Store) Accounts(ctx context.Context, provider string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider,id,email,plan,first_seen,last_seen FROM accounts WHERE provider=? ORDER BY last_seen DESC`, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var first, last int64
		if err := rows.Scan(&a.Provider, &a.ID, &a.Email, &a.Plan, &first, &last); err != nil {
			return nil, err
		}
		a.FirstSeen, a.LastSeen = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) cursor(ctx context.Context, path string) (offset, size int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT offset,size FROM file_cursors WHERE path=?`, path).Scan(&offset, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return offset, size, err
}

func (s *Store) putCursor(ctx context.Context, path string, offset, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO file_cursors(path,offset,size) VALUES(?,?,?)
		ON CONFLICT(path) DO UPDATE SET offset=excluded.offset,size=excluded.size`, path, offset, size)
	return err
}

func (s *Store) meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) setMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
