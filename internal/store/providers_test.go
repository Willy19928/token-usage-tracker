package store

import (
	"context"
	"path/filepath"
	"testing"
)

// A turn plan decides which requests the billing filter keeps, so caches
// keyed on the data revision must see it change.
func TestTurnPlansChangeTheDataRevision(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	revision := func() uint64 {
		t.Helper()
		value, err := s.DataRevision(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := revision()
	if err := s.PutTurnPlans(ctx, []TurnPlan{{SessionID: "s", TurnID: "t"}}); err != nil {
		t.Fatal(err)
	}
	unset := revision()
	if unset <= before {
		t.Fatalf("revision %d after a new plan, was %d", unset, before)
	}
	if err := s.PutTurnPlans(ctx, []TurnPlan{{SessionID: "s", TurnID: "t", Plan: "pro"}}); err != nil {
		t.Fatal(err)
	}
	named := revision()
	if named <= unset {
		t.Fatalf("revision %d after naming the plan, was %d", named, unset)
	}
	// A plan that is already named stays, and nothing changes.
	if err := s.PutTurnPlans(ctx, []TurnPlan{{SessionID: "s", TurnID: "t"}}); err != nil {
		t.Fatal(err)
	}
	if got := revision(); got != named {
		t.Fatalf("revision %d after an unchanged plan, want %d", got, named)
	}
}
