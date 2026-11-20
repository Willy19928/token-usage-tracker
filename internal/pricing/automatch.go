package pricing

import "strings"

// AutoMatch finds the built-in model that a relay's model name stands for.
// Relays put routing parts around the model's own name ("relay/claude-opus-5.5:thinking",
// "pool/auto-claude-opus-5-5"), so the name is reduced to the part after the
// last "/" and before the first ":", and then the longest ending that starts
// after a "-", "_" or "." and that builtIn knows is taken. builtIn returns the
// built-in model a name prices as. A name that only starts with a built-in
// model ("gpt-6.1-sol-mini") is not matched: it may be a different model.
func AutoMatch(name string, builtIn func(string) (string, bool)) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(name))
	if index := strings.LastIndex(s, "/"); index >= 0 {
		s = s[index+1:]
	}
	if index := strings.Index(s, ":"); index >= 0 {
		s = s[:index]
	}
	for s != "" {
		if target, ok := builtIn(s); ok {
			return target, true
		}
		index := strings.IndexAny(s, "-_.")
		if index < 0 {
			break
		}
		s = s[index+1:]
	}
	return "", false
}

// AutoTarget returns the built-in Codex model that an unknown model name
// automatically maps to. A built-in name maps to nothing: it is priced as itself.
// The local codex-auto-review policy is never a target.
func AutoTarget(model string) (string, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if _, ok := resolveBuiltIn(model); ok {
		return "", false
	}
	return AutoMatch(model, func(name string) (string, bool) {
		entry, ok := resolveBuiltIn(name)
		if !ok || entry.Model == "codex-auto-review" {
			return "", false
		}
		return entry.Model, true
	})
}

// AutoMapping is a model name that maps to a built-in model automatically, as
// shown in the pricing dialog. Overridden is set while a saved override decides
// the model's price instead.
type AutoMapping struct {
	Model      string `json:"model"`
	Target     string `json:"target"`
	Overridden bool   `json:"overridden"`
	Requests   int64  `json:"requests"`
	Tokens     int64  `json:"tokens"`
}

// RelayName reports a model name that marks a request made through a
// third-party API: relays name models with routing parts, a "/" or ":"
// ("relay/claude-opus-5.5:thinking") or a built-in model name inside a longer one
// ("auto-claude-opus-5-5"). auto is the source's automatic mapping. A relay
// that passes official names through is not detected by name.
func RelayName(name string, auto func(string) (string, bool)) bool {
	if strings.ContainsAny(name, "/:") {
		return true
	}
	_, ok := auto(name)
	return ok
}

// CodexRelayName is RelayName for Codex model names.
func CodexRelayName(name string) bool { return RelayName(name, AutoTarget) }
