package usage

import "testing"

func TestResponseBoundaryMirrorDoesNotCreateLegacyEvent(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		st, home, path := accountingFixture(t)
		first := usage(33159, 4864, 0, 135, 68, 33294)
		second := usage(40505, 32512, 0, 221, 149, 40726)
		appendAccounting(t, path, accountingMeta+accountingTurn("one")+
			responseLine("scope-test", "one", "first", "2026-10-01T00:00:00Z", first, first)+
			tokenLine("2026-10-01T00:00:01Z", first, first)+"\n")
		if incremental {
			scanResponses(t, st, home)
		}
		appendAccounting(t, path, accountingTurn("two")+tokenLine("2026-10-01T00:01:00Z", first, first)+"\n")
		if incremental {
			scanResponses(t, st, home)
			accountingTotal(t, st, 33294)
		}
		appendAccounting(t, path, responseLine("scope-test", "two", "second", "2026-10-01T00:01:01Z", second, second)+
			tokenLine("2026-10-01T00:01:02Z", usage(73664, 37376, 0, 356, 217, 74020), second)+"\n")
		scanResponses(t, st, home)
		accountingTotal(t, st, 74020)
		if r := scanResponses(t, st, home); r.EventsInserted != 0 {
			t.Fatal(r)
		}
	}
}

func TestEqualVectorIndependentResponseStillCountsAcrossTurn(t *testing.T) {
	st, home, path := accountingFixture(t)
	u := usage(100, 20, 0, 5, 1, 105)
	appendAccounting(t, path, accountingMeta+accountingTurn("one")+
		responseLine("scope-test", "one", "a", "2026-10-01T00:00:00Z", u, u)+tokenLine("2026-10-01T00:00:01Z", u, u)+"\n")
	scanResponses(t, st, home)
	appendAccounting(t, path, accountingTurn("two")+tokenLine("2026-10-01T00:01:00Z", u, u)+"\n"+
		responseLine("scope-test", "two", "b", "2026-10-01T00:01:01Z", u, u))
	scanResponses(t, st, home)
	accountingTotal(t, st, 210)
}
