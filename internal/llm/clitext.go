// Package llm — text helpers shared by the subprocess CLI providers.
//
// The `claudecli` and `codex` providers both shell out to an external
// coding-agent binary that has no native structured-output mechanism.
// They get the expand / rerank / verify / tool-call JSON shapes the
// same way: append a JSON-Schema instruction to the prompt, then
// extract the first balanced JSON value out of a possibly chatty
// response. That logic is provider-neutral, so it lives here once
// rather than being copied into each provider package.
package llm

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// AppendSchemaInstruction tacks a "respond with exactly this JSON
// shape" rider onto a system / instruction prompt. The CLI providers
// have no native structured-output flag, so this — paired with
// ExtractJSON on the response — is how the structured shapes are
// enforced. Returns the prompt unchanged for ShapeFreeform (or if the
// schema fails to marshal, which the hand-built schemas never do).
func AppendSchemaInstruction(prompt string, shape JSONShape, tools []ToolSpec) string {
	schema := JSONSchemaFor(shape, tools)
	if schema == nil {
		return prompt
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return prompt
	}
	rider := "Respond with a single JSON object that conforms exactly to this JSON Schema:\n" +
		string(raw) +
		"\nOutput ONLY the JSON object — no prose, no commentary, no markdown fences."
	if strings.TrimSpace(prompt) == "" {
		return rider
	}
	return prompt + "\n\n" + rider
}

// ExtractJSON pulls the first balanced JSON object or array out of
// text. CLI agents sometimes wrap responses in markdown fences or
// surround them with prose ("Sure, here you go:\n{...}"); this helper
// finds and verifies the JSON payload so the assist passes don't choke
// on chatty completions. ok is false when no valid JSON value is
// present.
func ExtractJSON(text string) (string, bool) {
	if c, ok := tryUnmarshalJSON(text); ok {
		return c, true
	}
	if stripped, changed := stripJSONFences(text); changed {
		if c, ok := tryUnmarshalJSON(stripped); ok {
			return c, true
		}
		text = stripped
	}
	// Scan for the first balanced JSON object/array.
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '{' && c != '[' {
			continue
		}
		end, ok := balancedJSONEnd(text, i)
		if !ok {
			continue
		}
		candidate := text[i : end+1]
		if c, ok := tryUnmarshalJSON(candidate); ok {
			return c, true
		}
	}
	return "", false
}

// FailureSnippet picks the most informative blob to attach to a failed
// CLI spawn. stderr comes first — that is where a well-behaved CLI puts
// its diagnostics — but the coding-agent CLIs print terminal errors on
// stdout in print mode (`claude -p` reports "Error: Reached max turns
// (1)" there and exits 1 with an empty stderr), and an empty stderr
// must not collapse a real message into a bare "exit status 1".
func FailureSnippet(stdout, stderr []byte) string {
	if s := Snippet(stderr); s != "" {
		return s
	}
	return Snippet(stdout)
}

// Snippet truncates a stderr / stdout blob for inclusion in an error
// message. Operates on runes so multi-byte characters at the cut point
// stay intact.
func Snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	count := 0
	for i := range s {
		count++
		if count > max {
			return s[:i] + "…"
		}
	}
	return s
}

// stripJSONFences removes a single ``` or ```json wrapper.
func stripJSONFences(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") {
		return text, false
	}
	_, body, ok := strings.Cut(t, "\n")
	if !ok {
		return text, false
	}
	if i := strings.LastIndex(body, "```"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimSpace(body), true
}

// tryUnmarshalJSON returns the trimmed candidate if it parses as JSON.
func tryUnmarshalJSON(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", false
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return "", false
	}
	return trimmed, true
}

// balancedJSONEnd returns the index of the closing brace/bracket that
// balances the opener at start. Tracks string literals so quoted
// braces don't throw the depth count off.
func balancedJSONEnd(text string, start int) (int, bool) {
	open := text[start]
	var closer byte
	switch open {
	case '{':
		closer = '}'
	case '[':
		closer = ']'
	default:
		return 0, false
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}
