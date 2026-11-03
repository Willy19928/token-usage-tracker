package ledger

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// RateCard is one built-in rate row in USD per million tokens, as shown in the
// pricing dialog. CacheWrite1h is set only where the source prices it apart.
type RateCard struct {
	Model           string   `json:"model"`
	ValidFrom       string   `json:"valid_from"`
	MinPromptTokens int64    `json:"min_prompt_tokens"`
	Input           float64  `json:"input"`
	Output          float64  `json:"output"`
	CacheRead       float64  `json:"cache_read"`
	CacheWrite      float64  `json:"cache_write"`
	CacheWrite1h    *float64 `json:"cache_write_1h,omitempty"`
	Source          string   `json:"source"`
}

// UnpricedModel groups saved requests that no rate covers, by model and reason.
type UnpricedModel struct {
	Model      string `json:"model"`
	ReasonKind string `json:"reason_kind"`
	Reason     string `json:"reason"`
	Requests   int64  `json:"requests"`
	Tokens     int64  `json:"tokens"`
}

// PricingInfo is a ledger's rate catalog, user overrides and unpriced usage.
type PricingInfo struct {
	Provider      string          `json:"provider"`
	Label         string          `json:"label"`
	Currency      string          `json:"currency"`
	Separate1h    bool            `json:"separate_1h_cache_write"`
	Catalog       []RateCard      `json:"catalog"`
	Overrides     []DatedOverride `json:"overrides"`
	Unpriced      []UnpricedModel `json:"unpriced_models"`
	Notes         []string        `json:"notes"`
	PricedTokens  int64           `json:"priced_tokens"`
	UnpricedTotal int64           `json:"unpriced_tokens"`
}

const (
	anthropicPricingSource = "https://platform.claude.com/docs/en/about-claude/pricing"
	geminiPricingSource    = "https://ai.google.dev/gemini-api/docs/pricing"
)

func claudeCatalog() []RateCard {
	out := make([]RateCard, 0, len(claudeRateCards))
	for _, c := range claudeRateCards {
		write1h := c.Write1h
		validFrom := taipeiDay(c.ValidFrom)
		if c.ValidFrom.Equal(claudeLaunchRate) {
			validFrom = "" // unchanged since the model's release
		}
		out = append(out, RateCard{
			Model: c.Model, ValidFrom: validFrom, MinPromptTokens: c.MinPromptTokens,
			Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite, CacheWrite1h: &write1h,
			Source: anthropicPricingSource,
		})
	}
	return sortCatalog(out)
}

func antigravityCatalog() []RateCard {
	var out []RateCard
	for modelName, cards := range agRateCards {
		source := geminiPricingSource
		if strings.HasPrefix(modelName, "claude-") {
			source = anthropicPricingSource
		}
		for _, c := range cards {
			validFrom := taipeiDay(c.From)
			if c.From.Equal(claudeLaunchRate) {
				validFrom = "" // unchanged since the model's release
			}
			out = append(out, RateCard{
				Model: modelName, ValidFrom: validFrom,
				Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite, Source: source,
			})
			if modelName == "gemini-3.1-pro" {
				out = append(out, RateCard{
					Model: modelName, ValidFrom: taipeiDay(c.From), MinPromptTokens: 200001,
					Input: 4, Output: 18, CacheRead: .4, Source: source,
				})
			}
		}
	}
	return sortCatalog(out)
}

func sortCatalog(rows []RateCard) []RateCard {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		if rows[i].ValidFrom != rows[j].ValidFrom {
			return rows[i].ValidFrom < rows[j].ValidFrom
		}
		return rows[i].MinPromptTokens < rows[j].MinPromptTokens
	})
	return rows
}

// Pricing reports the built-in rates, saved overrides and currently unpriced
// usage of this ledger.
func (p *Provider) Pricing(ctx context.Context) PricingInfo {
	info := PricingInfo{Provider: p.Name, Label: p.Label, Currency: "USD", Overrides: p.ledger.overrides(ctx)}
	switch p.Name {
	case ProviderClaude:
		info.Catalog = claudeCatalog()
		info.Separate1h = true
		info.Notes = []string{"fast", "us_region", "haiku_long_context"}
	case ProviderAntigravity:
		info.Catalog = antigravityCatalog()
		info.Notes = []string{"gemini_long_context", "routing_model"}
	}
	if info.Overrides == nil {
		info.Overrides = []DatedOverride{}
	}
	groups := map[[2]string]*UnpricedModel{}
	for _, e := range p.Entries() {
		if e.Price.Priced {
			info.PricedTokens += e.Usage.Total
			continue
		}
		info.UnpricedTotal += e.Usage.Total
		key := [2]string{e.Model, e.Price.ReasonKind}
		group := groups[key]
		if group == nil {
			group = &UnpricedModel{Model: e.Model, ReasonKind: e.Price.ReasonKind, Reason: e.Price.Reason}
			groups[key] = group
		}
		group.Requests++
		group.Tokens += e.Usage.Total
	}
	info.Unpriced = make([]UnpricedModel, 0, len(groups))
	for _, group := range groups {
		info.Unpriced = append(info.Unpriced, *group)
	}
	sort.Slice(info.Unpriced, func(i, j int) bool { return info.Unpriced[i].Tokens > info.Unpriced[j].Tokens })
	return info
}

// SetOverrides validates and replaces this ledger's user-entered rates, then
// reprices every saved request.
func (p *Provider) SetOverrides(ctx context.Context, rows []DatedOverride) error {
	clean := make([]DatedOverride, 0, len(rows))
	for index, row := range rows {
		row.Model = strings.TrimSpace(row.Model)
		if row.Model == "" {
			return fmt.Errorf("第 %d 筆覆寫缺少模型名稱", index+1)
		}
		if _, err := time.Parse("2006-01-02", row.ValidFrom); err != nil {
			return fmt.Errorf("%s 的生效日必須是 YYYY-MM-DD", row.Model)
		}
		if row.ValidTo != nil {
			if *row.ValidTo == "" {
				row.ValidTo = nil
			} else if _, err := time.Parse("2006-01-02", *row.ValidTo); err != nil || *row.ValidTo <= row.ValidFrom {
				return fmt.Errorf("%s 的結束日必須是晚於生效日的 YYYY-MM-DD", row.Model)
			}
		}
		rates := []float64{row.Input, row.Output, row.CacheRead, row.CacheWrite}
		if row.CacheWrite1h != nil {
			rates = append(rates, *row.CacheWrite1h)
		}
		for _, rate := range rates {
			if rate < 0 || rate > 1e6 || math.IsNaN(rate) || math.IsInf(rate, 0) {
				return fmt.Errorf("%s 的單價必須是 0 到 1,000,000 之間的數字", row.Model)
			}
		}
		if row.MinPromptTokens < 0 {
			return fmt.Errorf("%s 的提示門檻不能是負數", row.Model)
		}
		clean = append(clean, row)
	}
	tx, err := p.ledger.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := putState(ctx, tx, "pricing-overrides", clean); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return p.Reload(ctx)
}
