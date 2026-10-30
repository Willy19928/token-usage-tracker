package usage

import (
	"context"
	"fmt"
	"testing"

	"tokenusage/internal/model"
)

func TestLateTurnModelRepairsOnlyUnknownMatchingTurn(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		t.Run(fmt.Sprint(incremental), func(t *testing.T) {
			st, home, path := accountingFixture(t)
			appendAccounting(t, path, accountingMeta+responseN("scope-test", "pending", "a", 100, 100)+responseN("scope-test", "other", "b", 200, 200))
			if incremental {
				scanResponses(t, st, home)
			}
			appendAccounting(t, path, `{"type":"turn_context","payload":{"turn_id":"pending","model":"gpt-6.1-sol"}}`+"\n")
			scanResponses(t, st, home)
			// A later model switch in the same turn must not rewrite earlier known requests.
			appendAccounting(t, path, accountingTurn("pending"))
			scanResponses(t, st, home)
			models := map[string]string{}
			err := st.WalkEvents(context.Background(), model.Filter{}, func(e model.UsageEvent) error {
				models[e.TurnID] = e.Model
				return nil
			})
			if err != nil || models["pending"] != "gpt-6.1-sol" || models["other"] != "" || len(models) != 2 {
				t.Fatalf("models=%v error=%v", models, err)
			}
			accountingTotal(t, st, 300)
		})
	}
}
