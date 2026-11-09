package limits

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// Collection states shown on the page.
const (
	StateIdle      = "idle"
	StateRunning   = "running"
	StateOK        = "ok"
	StateDisabled  = "disabled"
	StateSignedOut = "signed_out"
	StateClosed    = "closed"
	StateError     = "error"
)

// Status describes the latest collection attempt for one provider.
type Status struct {
	State       string    `json:"state"`
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type collector interface {
	Collect(ctx context.Context) error
}

// Manager runs the collectors on their own schedules.
type Manager struct {
	Store         *Store
	Codex         *CodexCollector
	Claude        *ClaudeCollector
	Antigravity   *AntigravityCollector
	ClaudeEnabled func() bool

	mu       sync.Mutex
	statuses map[string]Status
	running  map[string]*sync.Mutex
	wake     chan string
}

func (m *Manager) init() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statuses == nil {
		m.statuses = map[string]Status{}
		m.running = map[string]*sync.Mutex{Codex: {}, Claude: {}, Antigravity: {}}
		m.wake = make(chan string, 4)
	}
}

// Run collects until ctx ends: Codex every minute, Antigravity every two
// minutes and Claude every five minutes.
func (m *Manager) Run(ctx context.Context) {
	m.init()
	schedules := map[string]time.Duration{Codex: time.Minute, Antigravity: 2 * time.Minute, Claude: 5 * time.Minute}
	var wg sync.WaitGroup
	wakes := map[string]chan struct{}{}
	for provider := range schedules {
		wakes[provider] = make(chan struct{}, 1)
	}
	for provider, interval := range schedules {
		wg.Add(1)
		go func(provider string, interval time.Duration, wake <-chan struct{}) {
			defer wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				m.Collect(ctx, provider)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-wake:
				}
			}
		}(provider, interval, wakes[provider])
	}
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case provider := <-m.wake:
			if ch, ok := wakes[provider]; ok {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}
}

// Wake asks for an immediate collection, for example after enabling Claude.
func (m *Manager) Wake(provider string) {
	m.init()
	select {
	case m.wake <- provider:
	default:
	}
}

// Collect runs one provider's collector now and records the outcome.
func (m *Manager) Collect(ctx context.Context, provider string) {
	m.init()
	var c collector
	switch provider {
	case Codex:
		c = m.Codex
	case Claude:
		if m.ClaudeEnabled != nil && !m.ClaudeEnabled() {
			m.setStatus(provider, func(s *Status) { s.State, s.Error = StateDisabled, "" })
			return
		}
		c = m.Claude
	case Antigravity:
		c = m.Antigravity
	}
	if c == nil {
		return
	}
	lock := m.running[provider]
	if !lock.TryLock() {
		return
	}
	defer lock.Unlock()
	started := time.Now().UTC()
	// The first pass can take minutes, for example a Codex backfill.
	m.setStatus(provider, func(s *Status) {
		if s.LastSuccess.IsZero() {
			s.State = StateRunning
		}
	})
	err := c.Collect(ctx)
	if errors.Is(err, context.Canceled) {
		return
	}
	m.setStatus(provider, func(s *Status) {
		s.LastAttempt = started
		switch {
		case err == nil:
			s.State, s.Error, s.LastSuccess = StateOK, "", started
		case errors.Is(err, ErrClaudeSignedOut):
			s.State, s.Error = StateSignedOut, ""
		case errors.Is(err, ErrAntigravityClosed):
			s.State, s.Error = StateClosed, ""
		default:
			s.State, s.Error = StateError, err.Error()
			log.Printf("%s limits: %v", provider, err)
		}
	})
}

func (m *Manager) setStatus(provider string, update func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := m.statuses[provider]
	update(&status)
	m.statuses[provider] = status
}

// Status reports the latest attempt for provider.
func (m *Manager) Status(provider string) Status {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	status, ok := m.statuses[provider]
	if !ok {
		status.State = StateIdle
	}
	return status
}
