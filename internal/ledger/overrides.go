package ledger

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"tokenusage/internal/pricing"
)

// DatedOverride is a user-entered date-effective rate (USD per million tokens)
// kept in the ledger state table. valid_to is exclusive.
type DatedOverride struct {
	Model string `json:"model"`
	// AliasOf prices the model as this built-in model, following its dated
	// rates. Unpriced keeps the model unpriced instead of mapping it
	// automatically. Rows with either carry no dates, threshold or rates.
	AliasOf         string   `json:"alias_of,omitempty"`
	Unpriced        bool     `json:"unpriced,omitempty"`
	ValidFrom       string   `json:"valid_from"`
	ValidTo         *string  `json:"valid_to"`
	MinPromptTokens int64    `json:"min_prompt_tokens"`
	Input           float64  `json:"input"`
	Output          float64  `json:"output"`
	CacheRead       float64  `json:"cache_read"`
	CacheWrite      float64  `json:"cache_write"`
	CacheWrite1h    *float64 `json:"cache_write_1h,omitempty"`
}

var dottedClaude = regexp.MustCompile(`^(claude-(?:opus|sonnet)-)(\d)\.(\d)$`)

// canonicalModel matches the shared dashboard canonicalization, so the same
// Claude model reported by Claude Code and by Antigravity merges into one key.
func canonicalModel(name string) string {
	return dottedClaude.ReplaceAllString(strings.ToLower(name), "$1$2-$3")
}

func (l *ledgerDB) overrides(ctx context.Context) []DatedOverride {
	var rows []DatedOverride
	if found, err := l.state(ctx, "pricing-overrides", &rows); err != nil || !found {
		return nil
	}
	return rows
}

// pickOverride selects the newest effective row whose prompt threshold is met.
func pickOverride(rows []DatedOverride, modelName, day string, prompt int64, canonical func(string) string) *DatedOverride {
	var matches []DatedOverride
	for _, row := range rows {
		if row.AliasOf != "" || row.Unpriced {
			continue
		}
		if canonical(row.Model) != canonical(modelName) || row.ValidFrom > day || prompt < row.MinPromptTokens {
			continue
		}
		if row.ValidTo != nil && *row.ValidTo != "" && day >= *row.ValidTo {
			continue
		}
		matches = append(matches, row)
	}
	if len(matches) == 0 {
		return nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].ValidFrom != matches[j].ValidFrom {
			return matches[i].ValidFrom > matches[j].ValidFrom
		}
		return matches[i].MinPromptTokens > matches[j].MinPromptTokens
	})
	return &matches[0]
}

// How a model's price is chosen.
const (
	mapBuiltIn  = "builtin"  // the model has built-in rates
	mapAlias    = "alias"    // the user mapped it to a built-in model
	mapAuto     = "auto"     // its name automatically maps to a built-in model
	mapCustom   = "custom"   // the user entered dated rates
	mapUnpriced = "unpriced" // the user kept it unpriced
	mapNone     = "none"     // nothing prices it
)

// modelRules is how one ledger names its built-in models: canonical turns a
// reported name into a rate-card key, and builtIn reports keys with rates.
type modelRules struct {
	canonical func(string) string
	builtIn   func(string) bool
}

var claudeCardModels = func() map[string]bool {
	out := map[string]bool{}
	for _, card := range claudeRateCards {
		out[card.Model] = true
	}
	return out
}()

var (
	claudeRules = modelRules{
		canonical: func(name string) string { return claudeCanonical(strings.ToLower(strings.TrimSpace(name))) },
		builtIn:   func(key string) bool { return claudeCardModels[key] },
	}
	antigravityRules = modelRules{
		canonical: func(name string) string { return canonicalModel(strings.TrimSpace(name)) },
		builtIn:   func(key string) bool { _, ok := agRateCards[key]; return ok },
	}
)

func (p *Provider) rules() modelRules {
	if p.Name == ProviderAntigravity {
		return antigravityRules
	}
	return claudeRules
}

// auto returns the built-in model an unknown name maps to automatically.
func (m modelRules) auto(name string) (string, bool) {
	if m.builtIn(m.canonical(name)) {
		return "", false
	}
	return pricing.AutoMatch(name, func(part string) (string, bool) {
		key := m.canonical(part)
		return key, m.builtIn(key)
	})
}

// resolve returns the rate-card key that prices name and how it was chosen.
// Saved rows win over built-in rates and automatic mapping; a model with
// dated rates is priced only by them, and stays unpriced outside their dates.
func (m modelRules) resolve(rows []DatedOverride, name string) (string, string) {
	key := m.canonical(name)
	custom := false
	for _, row := range rows {
		if m.canonical(row.Model) != key {
			continue
		}
		switch {
		case row.Unpriced:
			return key, mapUnpriced
		case row.AliasOf != "":
			return row.AliasOf, mapAlias
		default:
			custom = true
		}
	}
	switch {
	case custom:
		return key, mapCustom
	case m.builtIn(key):
		return key, mapBuiltIn
	}
	if target, ok := m.auto(name); ok {
		return target, mapAuto
	}
	return key, mapNone
}

// claudeAPI reports a Claude Code request billed through an API. Claude Code
// records neither the server it talks to nor how it signed in, so only a
// relay's model name tells; other requests count as subscription.
func claudeAPI(name string) bool { return pricing.RelayName(name, claudeRules.auto) }
