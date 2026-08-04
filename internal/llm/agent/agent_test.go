package agent

import (
	"strings"
	"testing"
)

// The subprocess CLI providers have no native structured output, so the
// envelope a step comes back in is only as reliable as the model tier
// behind them. Small models wrap the single call in a one-element array
// often enough that strict decoding makes whole tiers unusable.
func TestParseToolCall_Envelopes(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantTool string
		wantArg  string
	}{
		{
			name:     "bare object",
			raw:      `{"tool":"search_symbols","args":{"query":"sqlc schema"}}`,
			wantTool: "search_symbols",
			wantArg:  "sqlc schema",
		},
		{
			name:     "array-wrapped single call",
			raw:      `[{"tool":"search_symbols","args":{"query":"sqlc schema"}}]`,
			wantTool: "search_symbols",
			wantArg:  "sqlc schema",
		},
		{
			name:     "array with several calls takes the first",
			raw:      `[{"tool":"search_symbols","args":{"query":"sqlc schema"}},{"tool":"read_file","args":{}}]`,
			wantTool: "search_symbols",
			wantArg:  "sqlc schema",
		},
		{
			name:     "leading whitespace",
			raw:      "\n  [{\"tool\":\"search_symbols\",\"args\":{\"query\":\"sqlc schema\"}}]\n",
			wantTool: "search_symbols",
			wantArg:  "sqlc schema",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			call, err := parseToolCall(tc.raw)
			if err != nil {
				t.Fatalf("parseToolCall: %v", err)
			}
			if call.Tool != tc.wantTool {
				t.Errorf("tool=%q want %q", call.Tool, tc.wantTool)
			}
			if got, _ := call.Args["query"].(string); got != tc.wantArg {
				t.Errorf("args[query]=%q want %q", got, tc.wantArg)
			}
		})
	}
}

func TestParseToolCall_MissingArgsDefaultsToEmptyMap(t *testing.T) {
	call, err := parseToolCall(`{"tool":"final_answer"}`)
	if err != nil {
		t.Fatalf("parseToolCall: %v", err)
	}
	if call.Args == nil {
		t.Fatal("Args must never be nil")
	}
	if len(call.Args) != 0 {
		t.Errorf("Args=%v want empty", call.Args)
	}
}

// Leniency about the envelope must not become leniency about the
// contents.
func TestParseToolCall_Rejects(t *testing.T) {
	for _, raw := range []string{
		``,
		`[]`,
		`[`,
		`not json`,
		`{"args":{"query":"x"}}`,
		`[{"args":{"query":"x"}}]`,
	} {
		if _, err := parseToolCall(raw); err == nil {
			t.Errorf("parseToolCall(%q) = nil error, want failure", raw)
		}
	}
}

func TestSystemPrompt_ForbidsArrayAndFenceWrapping(t *testing.T) {
	a := &Agent{
		names: []string{"search_symbols"},
		tools: map[string]Tool{"search_symbols": {Description: "search"}},
	}
	got := a.systemPrompt("")
	for _, want := range []string{"never wrapped in an array", "never inside markdown fences"} {
		if !strings.Contains(got, want) {
			t.Errorf("system prompt missing %q:\n%s", want, got)
		}
	}
}
