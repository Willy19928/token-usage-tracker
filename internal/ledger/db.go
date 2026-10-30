// Package ledger keeps the durable Claude Code and Antigravity usage ledgers.
// Both ledgers keep a stable SQLite layout (records, state and revision
// audit tables) and JSON payloads, so the saved history, revision audit rows
// and pricing overrides stay readable across versions.
package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type ledgerDB struct {
	db   *sql.DB
	path string
}

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

func openLedger(directory string, schema string) (*ledgerDB, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "usage.sqlite")
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
	return &ledgerDB{db: db, path: path}, nil
}

func (l *ledgerDB) Close() error { return l.db.Close() }

type rawRecord struct {
	ID      string
	Payload string
}

func (l *ledgerDB) records(ctx context.Context) ([]rawRecord, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id,payload FROM records`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rawRecord
	for rows.Next() {
		var item rawRecord
		if err := rows.Scan(&item.ID, &item.Payload); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// state reads one JSON document from the state table. found is false when the
// key has never been written.
func (l *ledgerDB) state(ctx context.Context, id string, out any) (bool, error) {
	var payload string
	err := l.db.QueryRowContext(ctx, `SELECT payload FROM state WHERE id=?`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(payload), out)
}

func putState(ctx context.Context, tx *sql.Tx, id string, value any) error {
	encoded, err := marshalJS(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO state(id,payload) VALUES(?,?)`, id, encoded)
	return err
}

func jsonUnmarshal(payload string, out any) error { return json.Unmarshal([]byte(payload), out) }

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// marshalJS encodes like JSON.stringify: no HTML escaping, no trailing newline.
func marshalJS(value any) (string, error) {
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(builder.String(), "\n"), nil
}

// jsISOTime mirrors Date.prototype.toISOString.
func jsISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseJSTime accepts the ISO forms Claude Code and Antigravity write.
func parseJSTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

var taipei = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		return time.FixedZone("Asia/Taipei", 8*3600)
	}
	return loc
}()

// taipeiDay is the calendar day used for date-effective rates.
func taipeiDay(value time.Time) string { return value.In(taipei).Format("2006-01-02") }
