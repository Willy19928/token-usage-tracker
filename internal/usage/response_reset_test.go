package usage

import (
	"context"
	"strings"
	"testing"
)

func withThreadTotal(line, total string) string {
	return strings.TrimSuffix(line, "}}\n") + `,"thread_token_usage":` + total + "}}\n"
}

func TestResponseTurnResetRequiresExactThreadBoundary(t *testing.T) {
	for _, verified := range []bool{true, false} {
		t.Run(map[bool]string{true: "verified", false: "unverified"}[verified], func(t *testing.T) {
			st, home, path := accountingFixture(t)
			first := usage(33159, 4864, 0, 135, 68, 33294)
			second := usage(40505, 32512, 0, 221, 149, 40726)
			boundary := usage(73664, 37376, 0, 356, 217, 74020)
			if !verified {
				boundary = usage(73665, 37376, 0, 356, 217, 74021)
			}
			a := withThreadTotal(responseLine("scope-test", "one", "a", "2026-10-01T00:00:00Z", first, first), first)
			b := withThreadTotal(responseLine("scope-test", "one", "b", "2026-10-01T00:00:01Z", second, second), boundary)
			// Older durable turns can precede a resumed producer's thread
			// counter; they must not invalidate the exact current-turn proof.
			appendAccounting(t, path, accountingMeta+accountingTurn("older")+responseN("scope-test", "older", "prior", 100, 100)+accountingTurn("one")+a+b)
			r := scanResponses(t, st, home)
			if !verified {
				accountingTotal(t, st, 33394)
				if r.Warnings == 0 {
					t.Fatal("unverified reset accepted")
				}
				return
			}
			if r.Warnings != 0 {
				t.Fatal(r)
			}
			accountingTotal(t, st, 74120)
			appendAccounting(t, path, b)
			if r = scanResponses(t, st, home); r.EventsInserted != 0 {
				t.Fatal(r)
			}
			// The restarted turn counter may stay below accounted usage for
			// several later requests; thread reconciliation must keep working.
			third := usage(1000, 800, 0, 10, 2, 1010)
			turn := usage(41505, 33312, 0, 231, 151, 41736)
			thread := usage(74664, 38176, 0, 366, 219, 75030)
			appendAccounting(t, path, withThreadTotal(responseLine("scope-test", "one", "c", "2026-10-01T00:00:02Z", third, turn), thread))
			if r = scanResponses(t, st, home); r.Warnings != 0 {
				t.Fatal(r)
			}
			accountingTotal(t, st, 75130)
		})
	}
}

func TestLateResponseThreadBoundaryDoesNotDoubleCount(t *testing.T) {
	st, home, path := accountingFixture(t)
	first := withThreadTotal(responseN("scope-test", "one", "later", 20, 50), usage(50, 0, 0, 0, 0, 50))
	late := withThreadTotal(responseN("scope-test", "one", "earlier", 30, 30), usage(30, 0, 0, 0, 0, 30))
	appendAccounting(t, path, accountingMeta+accountingTurn("one")+first)
	scanResponses(t, st, home)
	appendAccounting(t, path, late)
	if r := scanResponses(t, st, home); r.EventsInserted != 0 {
		t.Fatal(r)
	}
	accountingTotal(t, st, 50)
	u, err := st.SessionUsage(context.Background(), "scope-test")
	if err != nil || u.Total != 50 {
		t.Fatalf("%+v %v", u, err)
	}
}
