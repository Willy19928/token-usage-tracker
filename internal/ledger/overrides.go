package ledger

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// DatedOverride is a user-entered date-effective rate (USD per million tokens)
// kept in the ledger state table. valid_to is exclusive.
type DatedOverride struct {
	Model           string   `json:"model"`
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
