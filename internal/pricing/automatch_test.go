package pricing

import "testing"

func TestAutoTargetMapsRelayNamesToBuiltInModels(t *testing.T) {
	for name, want := range map[string]string{
		"relay/gpt-6.1-sol":                 "gpt-6.1-sol",
		"https-example-relay/gpt-6.2-astra": "gpt-6.2-astra",
		"pool/auto-gpt-5.4:thinking":        "gpt-5.4",
		"relay/gpt-5.4-mini":                "gpt-5.4-mini", // the longest ending wins
		"relay/gpt-6.1-sol-2026-11-12":      "gpt-6.1-sol",  // dated snapshots too
		"Relay/GPT-6.1-Sol":                 "gpt-6.1-sol",
	} {
		if got, ok := AutoTarget(name); !ok || got != want {
			t.Errorf("AutoTarget(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{
		"gpt-6.1-sol",      // built-in: priced as itself
		"gpt-6.1-sol-mini", // only starts with a built-in name
		"relay/internal-model",
		"relay/codex-auto-review", // the local zero-price policy is never a target
		"",
	} {
		if got, ok := AutoTarget(name); ok {
			t.Errorf("AutoTarget(%q) = %q, want no match", name, got)
		}
	}
}

func TestResolveAppliesAutoMappingUnlessOverridden(t *testing.T) {
	rate, found, err := Resolve("relay/gpt-6.1-sol", nil)
	if err != nil || !found || !rate.Auto || rate.Custom || rate.CanonicalModel != "gpt-6.1-sol" {
		t.Fatalf("auto rate = %+v found=%v err=%v", rate, found, err)
	}
	builtIn, _, _ := Resolve("gpt-6.1-sol", nil)
	if rate.InputNanoPerToken != builtIn.InputNanoPerToken || rate.OutputNanoPerToken != builtIn.OutputNanoPerToken {
		t.Fatalf("auto rate %+v differs from built-in %+v", rate, builtIn)
	}
	overrides, err := NormalizeOverrides(map[string]Override{
		"relay/gpt-6.1-sol":   {Unpriced: true},
		"relay/gpt-6.2-astra": {AliasOf: "gpt-6-luna"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := Resolve("relay/gpt-6.1-sol", overrides); err != nil || found {
		t.Fatalf("unpriced override still priced: found=%v err=%v", found, err)
	}
	if rate, found, err := Resolve("relay/gpt-6.2-astra", overrides); err != nil || !found || rate.Auto || rate.CanonicalModel != "gpt-6-luna" {
		t.Fatalf("manual alias = %+v found=%v err=%v", rate, found, err)
	}
	if _, err := NormalizeOverrides(map[string]Override{"x": {Unpriced: true, AliasOf: "gpt-6-luna"}}); err == nil {
		t.Fatal("unpriced with an alias was accepted")
	}
	if _, err := NormalizeOverrides(map[string]Override{"x": {Unpriced: true, InputUSDPerMillion: "1"}}); err == nil {
		t.Fatal("unpriced with a rate was accepted")
	}
}

func TestFastMultiplierFollowsAutoMapping(t *testing.T) {
	want, wantD, ok := FastMultiplier("gpt-6.1-sol")
	got, gotD, gotOK := FastMultiplier("relay/gpt-6.1-sol")
	if !ok || !gotOK || got != want || gotD != wantD {
		t.Fatalf("relay multiplier = %d/%d %v, want %d/%d", got, gotD, gotOK, want, wantD)
	}
}

func TestCodexRelayName(t *testing.T) {
	for name, want := range map[string]bool{
		"gpt-6.1-sol":            false,
		"relay/gpt-6.1-sol":      true,
		"gpt-6.1-sol:thinking":   true,
		"auto-gpt-6.1-sol":       true,
		"internal-review":        false, // unknown names are not guessed
		"codex-auto-review":      false,
		"gpt-6.1-sol-2026-11-12": false, // a dated snapshot is official
	} {
		if got := CodexRelayName(name); got != want {
			t.Errorf("CodexRelayName(%q) = %v, want %v", name, got, want)
		}
	}
}
