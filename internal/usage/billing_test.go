package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"tokenusage/internal/model"
	"tokenusage/internal/store"
)

func billingTotal(t *testing.T, st *store.Store, billing string) int64 {
	t.Helper()
	summary, err := st.Summary(context.Background(), model.Filter{Billing: billing})
	if err != nil {
		t.Fatal(err)
	}
	return summary.Usage.Total
}

// billingSession writes one session with one turn of n tokens. limits is the
// token_count's rate_limits JSON, or "" for none.
func billingSession(t *testing.T, home, name, provider, modelName, limits string, n int64) {
	t.Helper()
	meta := `{"type":"session_meta","payload":{"id":"` + name + `"`
	if provider != "" {
		meta += `,"model_provider":"` + provider + `"`
	}
	meta += "}}\n"
	turn := strings.Replace(accountingTurn("one"), `"gpt-5.4"`, `"`+modelName+`"`, 1)
	token := accountingToken(n, n)
	if limits != "" {
		token = strings.Replace(token, `"payload":{"type":"token_count",`, `"payload":{"type":"token_count","rate_limits":`+limits+`,`, 1)
	}
	appendAccounting(t, filepath.Join(home, "sessions", name+".jsonl"), meta+turn+token)
}

const (
	plusLimits = `{"limit_id":"codex","plan_type":"plus"}`
	noPlan     = `{"limit_id":"codex","plan_type":null}`
)

func TestBillingFilterSplitsSubscriptionAndAPIRequests(t *testing.T) {
	st, home, _ := accountingFixture(t)
	ctx := context.Background()
	billingSession(t, home, "plus-session", "openai", "gpt-5.4", plusLimits, 1)
	// The provider name does not tell: a session started on "openai" can be
	// served through a relay, which names no plan.
	billingSession(t, home, "relayed-session", "openai", "gpt-5.4", noPlan, 10)
	// A configured provider that serves a plan counts as subscription.
	billingSession(t, home, "pooled-session", "example-relay", "gpt-5.4", `{"plan_type":"team"}`, 100)
	// Without rate limits, the provider and the model name decide.
	billingSession(t, home, "relay-provider", "example-relay", "gpt-5.4", "", 1000)
	billingSession(t, home, "relay-name", "openai", "relay/gpt-5.4", "", 10000)
	billingSession(t, home, "older-session", "", "gpt-5.4", "", 100000)
	if _, err := (&Scanner{Store: st}).Scan(ctx, []string{home}, false); err != nil {
		t.Fatal(err)
	}
	all, sub, api := billingTotal(t, st, ""), billingTotal(t, st, "subscription"), billingTotal(t, st, "api")
	if all != 111111 || sub != 100101 || api != 11010 {
		t.Fatalf("all=%d subscription=%d api=%d", all, sub, api)
	}
}

func TestUnchangedRolloutBackfillsPlansAndProviderWithoutChangingUsage(t *testing.T) {
	st, home, _ := accountingFixture(t)
	ctx := context.Background()
	billingSession(t, home, "relayed-session", "openai", "gpt-5.4", noPlan, 100)
	billingSession(t, home, "provider-session", "example-relay", "gpt-5.4", "", 7)
	scan := &Scanner{Store: st}
	if _, err := scan.Scan(ctx, []string{home}, false); err != nil {
		t.Fatal(err)
	}
	if got := billingTotal(t, st, "api"); got != 107 {
		t.Fatalf("api total after the first scan = %d", got)
	}
	// Simulate a database filled by a version that kept neither.
	db, err := sql.Open("sqlite", st.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM turn_plans; DELETE FROM plan_backfills; UPDATE sessions SET model_provider=''; DELETE FROM provider_backfills`); err != nil {
		t.Fatal(err)
	}
	if got := billingTotal(t, st, "api"); got != 0 {
		t.Fatalf("api total before the backfill = %d", got)
	}
	if _, err := scan.Scan(ctx, []string{home}, false); err != nil {
		t.Fatal(err)
	}
	if got := billingTotal(t, st, "api"); got != 107 {
		t.Fatalf("api total after the backfill = %d", got)
	}
	accountingTotal(t, st, 107)
}

func TestPlanOf(t *testing.T) {
	for raw, want := range map[string]string{
		plusLimits:       "plus",
		noPlan:           "",
		`{"primary":{}}`: "",
	} {
		if got, ok := planOf([]byte(raw)); !ok || got != want {
			t.Errorf("planOf(%s) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "null", "not json"} {
		if _, ok := planOf([]byte(raw)); ok {
			t.Errorf("planOf(%q) reported rate limits", raw)
		}
	}
}
