package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"

	"modernc.org/sqlite"

	"tokenusage/internal/pricing"
)

// Billing splits Codex usage into subscription and API requests. Each
// response's rate limits name the ChatGPT plan it was served under; a
// response whose rate limits name no plan was billed through an API (a relay
// or an API key). Turns record what their responses named in turn_plans: the
// plan, or "-" for none. Requests of turns without rate limits fall back to
// the session's provider and the model name: Codex records the provider a
// session was started with ("openai", or the id of a configured provider such
// as a relay), and relays name models with routing parts.
const relaySessionsSQL = `SELECT session_id FROM sessions WHERE lower(model_provider) NOT IN ('','openai')`

const planUnset = "-"

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("tu_relay_model", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		name, _ := args[0].(string)
		if pricing.CodexRelayName(name) {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

func migrateProviders(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE sessions ADD COLUMN model_provider TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS provider_backfills(path TEXT PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS turn_plans(session_id TEXT NOT NULL, turn_id TEXT NOT NULL, plan TEXT NOT NULL, PRIMARY KEY(session_id,turn_id))`,
		`CREATE TABLE IF NOT EXISTS plan_backfills(path TEXT PRIMARY KEY)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// billingWhere restricts events to subscription or API requests.
func billingWhere(billing, alias string) string {
	api := "COALESCE((SELECT p.plan='" + planUnset + "' FROM turn_plans p WHERE p.session_id=" + alias + ".session_id AND p.turn_id=" + alias + ".turn_id)," +
		"(" + alias + ".session_id IN (" + relaySessionsSQL + ") OR tu_relay_model(" + alias + ".model)=1))"
	switch billing {
	case "subscription":
		return api + "=0"
	case "api":
		return api + "=1"
	}
	return ""
}

// PutModelProvider records the model provider a session's metadata names.
func (s *Store) PutModelProvider(ctx context.Context, id, provider string) error {
	provider = strings.TrimSpace(provider)
	if id == "" || provider == "" {
		return nil
	}
	_, err := s.writer().ExecContext(ctx, `UPDATE sessions SET model_provider=? WHERE session_id=? AND model_provider<>?`, provider, id, provider)
	return err
}

func (s *Store) ProviderChecked(ctx context.Context, path string) (bool, error) {
	var count int
	err := s.reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_backfills WHERE path=?`, path).Scan(&count)
	return count > 0, err
}

func (s *Store) MarkProviderChecked(ctx context.Context, path string) error {
	_, err := s.writer().ExecContext(ctx, `INSERT OR IGNORE INTO provider_backfills VALUES(?)`, path)
	return err
}

// TurnPlan is the plan a turn's responses named: a plan name, or "" when the
// rate limits named none.
type TurnPlan struct {
	SessionID, TurnID, Plan string
}

// PutTurnPlans records turn plans. A named plan wins over none, so a turn
// that ever named its plan counts as a subscription turn.
func (s *Store) PutTurnPlans(ctx context.Context, items []TurnPlan) error {
	if len(items) == 0 {
		return nil
	}
	tx, commit, rollback, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer rollback()
	for _, item := range items {
		if item.SessionID == "" || item.TurnID == "" {
			continue
		}
		plan := strings.TrimSpace(item.Plan)
		if plan == "" {
			plan = planUnset
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO turn_plans VALUES(?,?,?)
			ON CONFLICT(session_id,turn_id) DO UPDATE SET plan=excluded.plan
			WHERE turn_plans.plan='`+planUnset+`' AND excluded.plan<>'`+planUnset+`'`, item.SessionID, item.TurnID, plan); err != nil {
			return err
		}
	}
	return commit()
}

func (s *Store) PlanFileChecked(ctx context.Context, path string) (bool, error) {
	var count int
	err := s.reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM plan_backfills WHERE path=?`, path).Scan(&count)
	return count > 0, err
}

func (s *Store) MarkPlanFileChecked(ctx context.Context, path string) error {
	_, err := s.writer().ExecContext(ctx, `INSERT OR IGNORE INTO plan_backfills VALUES(?)`, path)
	return err
}
