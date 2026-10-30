package ledger

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"tokenusage/internal/model"
)

const (
	ProviderClaude      = "claude"
	ProviderAntigravity = "antigravity"
)

// Price is a cost already evaluated by a provider's own date-effective rate
// card, split into the same categories as the Codex estimator (nano-USD).
type Price struct {
	Priced                                  bool
	RegularNano, CachedNano, CacheWriteNano int64
	OutputNano                              int64
	ReasonKind, Reason                      string
}

// Entry is one saved request in the shape shared by every dashboard query.
type Entry struct {
	ID          string
	Session     string
	Timestamp   time.Time
	Model       string
	Mode        string
	Precision   string
	TokenSource string
	Usage       model.TokenUsage
	Price       Price
}

// ScanInfo is the latest scan summary kept in the ledger state table.
type ScanInfo map[string]any

type Provider struct {
	Name   string
	Label  string
	ledger *ledgerDB
	scan   func(ctx context.Context) error
	load   func(ctx context.Context) ([]Entry, error)

	scanMu   sync.Mutex
	mu       sync.RWMutex
	entries  []Entry
	revision atomic.Uint64
	busy     atomic.Bool
	lastErr  atomic.Value
}

func (p *Provider) Close() error { return p.ledger.Close() }
func (p *Provider) Path() string { return p.ledger.path }
func (p *Provider) Busy() bool   { return p.busy.Load() }

// Revision changes whenever the in-memory view of the ledger is reloaded.
func (p *Provider) Revision() uint64 { return p.revision.Load() }

func (p *Provider) LastError() string {
	if value, ok := p.lastErr.Load().(string); ok {
		return value
	}
	return ""
}

func (p *Provider) Entries() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.entries
}

// Reload rebuilds the in-memory view (including prices) from the ledger.
func (p *Provider) Reload(ctx context.Context) error {
	entries, err := p.load(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
	p.revision.Add(1)
	return nil
}

// Scan imports new local usage, then reloads. Concurrent calls collapse.
func (p *Provider) Scan(ctx context.Context) error {
	if !p.scanMu.TryLock() {
		return nil
	}
	defer p.scanMu.Unlock()
	p.busy.Store(true)
	defer p.busy.Store(false)
	err := p.scan(ctx)
	if err != nil {
		p.lastErr.Store(err.Error())
	} else {
		p.lastErr.Store("")
	}
	if reloadErr := p.Reload(ctx); err == nil {
		err = reloadErr
	}
	return err
}

func (p *Provider) ScanInfo(ctx context.Context) ScanInfo {
	var info ScanInfo
	if found, err := p.ledger.state(ctx, "scan", &info); err != nil || !found {
		return nil
	}
	return info
}

// usdNano converts tokens × USD-per-million into nano-USD.
func usdNano(tokens int64, perMillion float64) int64 {
	return int64(math.Round(float64(tokens) * perMillion * 1000))
}

func unpriced(kind, reason string) Price { return Price{ReasonKind: kind, Reason: reason} }

func jsonNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}
