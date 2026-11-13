package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"tokenusage/internal/model"
)

// Session points record the cumulative states of a session-wide counter whose
// usage is already in the ledger, so one conversation spread over several
// rollout files (resume, undo, restore checkpoint) is counted once per request
// whatever the order the files are read in.
//
// A row (point, last) means the request of size last that ended at point has
// been counted. A row (point, "") only says the usage up to point is accounted
// for. Both prove that point is a counted state of the session.

const sessionPointsSchema = `CREATE TABLE IF NOT EXISTS session_points (
	session_id TEXT NOT NULL,
	point TEXT NOT NULL,
	last TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(session_id, point, last)
)`

const sessionPointBackfillsSchema = `CREATE TABLE IF NOT EXISTS session_point_backfills (
	session_id TEXT PRIMARY KEY
)`

func pointKey(usage model.TokenUsage) string {
	return fmt.Sprintf("%d,%d,%d,%d,%d,%d", usage.Input, usage.CachedInput, usage.CacheWriteInput,
		usage.Output, usage.ReasoningOutput, usage.Total)
}

// RecordSessionPoint marks state as counted, and the request last that ended
// there when last is not zero.
func (s *Store) RecordSessionPoint(ctx context.Context, sessionID string, state, last model.TokenUsage) error {
	if sessionID == "" || state.IsZero() {
		return nil
	}
	lastKey := ""
	if !last.IsZero() {
		lastKey = pointKey(last)
	}
	_, err := s.writer().ExecContext(ctx, `INSERT OR IGNORE INTO session_points(session_id,point,last) VALUES(?,?,?)`,
		sessionID, pointKey(state), lastKey)
	return err
}

// SessionSnapshotKnown reports whether the request last ending at state was
// counted already.
func (s *Store) SessionSnapshotKnown(ctx context.Context, sessionID string, state, last model.TokenUsage) (bool, error) {
	return s.exists(ctx, `SELECT 1 FROM session_points WHERE session_id=? AND point=? AND last=?`,
		sessionID, pointKey(state), pointKey(last))
}

// SessionStateKnown reports whether the usage up to state was counted already.
func (s *Store) SessionStateKnown(ctx context.Context, sessionID string, state model.TokenUsage) (bool, error) {
	if state.IsZero() {
		return false, nil
	}
	return s.exists(ctx, `SELECT 1 FROM session_points WHERE session_id=? AND point=? LIMIT 1`,
		sessionID, pointKey(state))
}

func (s *Store) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var found int
	err := s.reader().QueryRowContext(ctx, query, args...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SessionPointsBackfilled reports whether the points of a session's files
// read before this table existed have been recorded.
func (s *Store) SessionPointsBackfilled(ctx context.Context, sessionID string) (bool, error) {
	return s.exists(ctx, `SELECT 1 FROM session_point_backfills WHERE session_id=?`, sessionID)
}

func (s *Store) MarkSessionPointsBackfilled(ctx context.Context, sessionID string) error {
	_, err := s.writer().ExecContext(ctx, `INSERT OR IGNORE INTO session_point_backfills(session_id) VALUES(?)`, sessionID)
	return err
}

// SessionFileCursor is how far one file of a session has been read.
type SessionFileCursor struct {
	Path   string
	Offset int64
}

// SessionFileCursors lists the files already read for a session.
func (s *Store) SessionFileCursors(ctx context.Context, sessionID string) ([]SessionFileCursor, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT path,offset FROM file_cursors WHERE session_id=? AND offset>0 ORDER BY path`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionFileCursor
	for rows.Next() {
		var cursor SessionFileCursor
		if err := rows.Scan(&cursor.Path, &cursor.Offset); err != nil {
			return nil, err
		}
		out = append(out, cursor)
	}
	return out, rows.Err()
}

func migrateSessionPoints(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		sessionPointsSchema,
		sessionPointBackfillsSchema,
		`ALTER TABLE file_cursors ADD COLUMN inherited_baseline INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	return nil
}

// ForgetSessionPointsForTest empties the point tables, as in a ledger written
// before they existed.
func (s *Store) ForgetSessionPointsForTest(ctx context.Context) error {
	for _, stmt := range []string{`DELETE FROM session_points`, `DELETE FROM session_point_backfills`} {
		if _, err := s.writer().ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
