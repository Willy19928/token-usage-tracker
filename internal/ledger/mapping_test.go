package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"tokenusage/internal/model"
)

func claudeModelLine(id, modelName string) string {
	return strings.Replace(claudeLine(id, "req_"+id, "2026-10-01T00:00:0"+id+"Z", ""), "claude-opus-5-5", modelName, 1)
}

func entryCost(e Entry) int64 {
	return e.Price.RegularNano + e.Price.CachedNano + e.Price.CacheWriteNano + e.Price.OutputNano
}

func entryOf(t *testing.T, p *Provider, modelName string) Entry {
	t.Helper()
	for _, e := range p.Entries() {
		if e.Model == canonicalModel(modelName) {
			return e
		}
	}
	t.Fatalf("no entry for %s", modelName)
	return Entry{}
}

func TestClaudeRelayNamesPriceAsTheBuiltInModel(t *testing.T) {
	p := openClaudeFixture(t,
		claudeModelLine("1", "claude-opus-5-5"),
		claudeModelLine("2", "relay/claude-opus-5.5:thinking"),
		claudeModelLine("3", "pool/auto-claude-opus-5-5"),
		claudeModelLine("4", "relay/claude-opus-5-5-preview"),
	)
	official := entryOf(t, p, "claude-opus-5-5")
	for _, name := range []string{"relay/claude-opus-5.5:thinking", "pool/auto-claude-opus-5-5"} {
		e := entryOf(t, p, name)
		if !e.Price.Priced || entryCost(e) != entryCost(official) || !e.API {
			t.Fatalf("%s = %+v api=%v, want the official cost %d", name, e.Price, e.API, entryCost(official))
		}
	}
	// Only a name that ends with a built-in model maps automatically.
	if e := entryOf(t, p, "relay/claude-opus-5-5-preview"); e.Price.Priced || e.Price.ReasonKind != "unverified_rate" {
		t.Fatalf("unknown variant = %+v", e.Price)
	}
	if official.API {
		t.Fatal("official model counted as API")
	}
	info := p.Pricing(context.Background())
	got := map[string]string{}
	for _, row := range info.AutoMapped {
		if row.Overridden || row.Requests != 1 {
			t.Fatalf("auto row = %+v", row)
		}
		got[row.Model] = row.Target
	}
	if len(got) != 2 || got["relay/claude-opus-5.5:thinking"] != "claude-opus-5-5" || got["pool/auto-claude-opus-5-5"] != "claude-opus-5-5" {
		t.Fatalf("auto mapped = %+v", info.AutoMapped)
	}
}

func TestClaudeManualMappingOverridesAutoMapping(t *testing.T) {
	ctx := context.Background()
	p := openClaudeFixture(t, claudeModelLine("1", "relay/claude-opus-5.5:thinking"), claudeModelLine("2", "claude-sonnet-5-5"))
	sonnet := entryOf(t, p, "claude-sonnet-5-5")
	if err := p.SetOverrides(ctx, []DatedOverride{{Model: "relay/claude-opus-5.5:thinking", AliasOf: "claude-sonnet-5-5"}}); err != nil {
		t.Fatal(err)
	}
	if e := entryOf(t, p, "relay/claude-opus-5.5:thinking"); !e.Price.Priced || entryCost(e) != entryCost(sonnet) {
		t.Fatalf("manual alias = %+v, want %d", e.Price, entryCost(sonnet))
	}
	if rows := p.Pricing(ctx).AutoMapped; len(rows) != 1 || !rows[0].Overridden {
		t.Fatalf("auto rows after alias = %+v", rows)
	}
	if err := p.SetOverrides(ctx, []DatedOverride{{Model: "relay/claude-opus-5.5:thinking", Unpriced: true}}); err != nil {
		t.Fatal(err)
	}
	if e := entryOf(t, p, "relay/claude-opus-5.5:thinking"); e.Price.Priced || e.Price.ReasonKind != "manual_unpriced" {
		t.Fatalf("manual unpriced = %+v", e.Price)
	}
	// Removing the manual row restores the automatic mapping.
	if err := p.SetOverrides(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if e := entryOf(t, p, "relay/claude-opus-5.5:thinking"); !e.Price.Priced {
		t.Fatalf("restored mapping = %+v", e.Price)
	}
	for _, rows := range [][]DatedOverride{
		{{Model: "x", AliasOf: "claude-sonnet-5-5", Unpriced: true}},
		{{Model: "x", AliasOf: "not-a-model"}},
		{{Model: "claude-opus-5-5", AliasOf: "claude-sonnet-5-5"}},
		{{Model: "x", Unpriced: true}, {Model: "x", AliasOf: "claude-sonnet-5-5"}},
		{{Model: "x", Unpriced: true}, {Model: "x", ValidFrom: "2026-01-01", Input: 1}},
		{{Model: "x", ValidFrom: "2026-01-01", Input: 1}, {Model: "x", Unpriced: true}},
	} {
		if err := p.SetOverrides(ctx, rows); err == nil {
			t.Errorf("accepted %+v", rows)
		}
	}
}

func TestBillingFilterSplitsClaudeAPIRequests(t *testing.T) {
	p := openClaudeFixture(t, claudeModelLine("1", "claude-opus-5-5"), claudeModelLine("2", "relay/claude-opus-5.5:thinking"))
	loc := time.UTC
	if got := len(p.Select(model.Filter{}, loc)); got != 2 {
		t.Fatalf("all = %d", got)
	}
	if got := p.Select(model.Filter{Billing: "subscription"}, loc); len(got) != 1 || got[0].Model != "claude-opus-5-5" {
		t.Fatalf("official = %+v", got)
	}
	if got := p.Select(model.Filter{Billing: "api"}, loc); len(got) != 1 || got[0].Model != "relay/claude-opus-5.5:thinking" {
		t.Fatalf("relay = %+v", got)
	}
}

func TestAntigravityRelayNamesPriceAsTheBuiltInModel(t *testing.T) {
	at := mustDate("2026-11-20T00:00:00Z")
	record := func(name string) AntigravityRecord {
		return AntigravityRecord{Model: name, Totals: agTotals{Input: 1000, Output: 100, Total: 1100}}
	}
	official := priceAntigravity(record("gemini-4-neon"), at, nil)
	relayed := priceAntigravity(record("relay-gemini-4-neon"), at, nil)
	if !official.Priced || relayed != official {
		t.Fatalf("relay price = %+v, official = %+v", relayed, official)
	}
	if p := priceAntigravity(record("relay-gemini-4-neon"), at, []DatedOverride{{Model: "relay-gemini-4-neon", Unpriced: true}}); p.Priced || p.ReasonKind != "manual_unpriced" {
		t.Fatalf("manual unpriced = %+v", p)
	}
	hydrogen := priceAntigravity(record("gemini-4-hydrogen"), at, nil)
	if p := priceAntigravity(record("relay-gemini-4-neon"), at, []DatedOverride{{Model: "relay-gemini-4-neon", AliasOf: "gemini-4-hydrogen"}}); p != hydrogen {
		t.Fatalf("manual alias = %+v, want %+v", p, hydrogen)
	}
}
