package hooks

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPromptQuery(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"empty", "", ""},
		{"whitespace", "   \n  ", ""},
		{"slash command", "/clear", ""},
		{"short ack", "ok", ""},
		{"two-letter word", "go", ""},
		{"normal task", "fix the auth bug", "fix the auth bug"},
		{"collapses whitespace", "fix\n\n  the   bug", "fix the bug"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, promptQuery(tc.prompt))
		})
	}
}

func TestPromptQueryTruncatesLongPrompts(t *testing.T) {
	long := strings.Repeat("token ", 100) // ~600 chars
	got := promptQuery(long)
	require.LessOrEqual(t, len([]rune(got)), 240)
	require.NotEmpty(t, got)
}

func TestBuildPromptInjection(t *testing.T) {
	require.Equal(t, "", buildPromptInjection(nil), "no hits → no block")

	hits := []grepSymbolHit{
		{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
		{Name: "AuthMiddleware", Kind: "type", FilePath: "internal/auth/mw.go"}, // no line
	}
	block := buildPromptInjection(hits)
	require.Contains(t, block, "relevant indexed symbols")
	require.Contains(t, block, "`ValidateToken` (function) — internal/auth/token.go:42")
	require.Contains(t, block, "`AuthMiddleware` (type) — internal/auth/mw.go")
	// The follow-up routing points at the one-shot localization verb first,
	// with the granular readers as the single-symbol fallback.
	require.Contains(t, block, "`explore`")
	require.Contains(t, block, `read(operation:"source")`)
}

func TestBuildPromptInjectionCapsHits(t *testing.T) {
	var hits []grepSymbolHit
	for i := 0; i < 20; i++ {
		hits = append(hits, grepSymbolHit{Name: "Sym", Kind: "function", FilePath: "f.go", Line: i + 1})
	}
	block := buildPromptInjection(hits)
	require.Equal(t, maxInjectedHits, strings.Count(block, "- `Sym`"), "injection is capped")
}

// TestRunUserPromptSubmitTrivialPromptIsNoOp confirms a trivial prompt never
// even probes the daemon (and so produces no output).
func TestRunUserPromptSubmitTrivialPromptIsNoOp(t *testing.T) {
	// A slash command produces an empty query → early return, no panic, no
	// daemon dial. We assert it doesn't panic and returns cleanly.
	runUserPromptSubmit([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"/clear"}`))
	runUserPromptSubmit([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"ok"}`))
	// Wrong event name is also a no-op.
	runUserPromptSubmit([]byte(`{"hook_event_name":"SessionStart","prompt":"do something big"}`))
}

func TestFramedLocalizationProblemStatementPreservesOnlyFramedProblem(t *testing.T) {
	prompt := "runner metadata\r\n" +
		"--- BUG REPORT --------------------------------\r\n" +
		"\r\n" +
		"Title: Queue entries vanish\r\n" +
		"\r\n" +
		"Path: internal/queue/store.go\r\n" +
		"  keep  interior spacing\r\n" +
		"\r\n" +
		"------------------------------------------------\r\n" +
		"Do not edit. FILES / SYMBOLS / EVIDENCE only."

	got, ok := framedLocalizationProblemStatement(prompt)
	require.True(t, ok)
	require.Equal(t, "Title: Queue entries vanish\n\nPath: internal/queue/store.go\n  keep  interior spacing", got)
	require.NotContains(t, got, "runner metadata")
	require.NotContains(t, got, "Do not edit")
}

func TestFramedLocalizationProblemStatementAcceptsApprovedLabels(t *testing.T) {
	labels := []string{"BUG", "ISSUE REPORT", "problem details", "Production Incident", "FAILURE", "change request"}
	for _, label := range labels {
		t.Run(label, func(t *testing.T) {
			prompt := "=== " + label + "\n\nTitle: Neutral storage behavior\n\n================"
			got, ok := framedLocalizationProblemStatement(prompt)
			require.True(t, ok)
			require.Equal(t, "Title: Neutral storage behavior", got)
		})
	}
}

func TestFramedLocalizationProblemStatementFailsOpen(t *testing.T) {
	valid := "--- BUG ---\nTitle: One\n----------------"
	tests := map[string]string{
		"absent":             "Title: One without framing",
		"unapproved label":   "--- TASK ---\nTitle: One\n----------------",
		"unsupported fence":  "___ BUG ___\nTitle: One\n________________",
		"mismatched closer":  "--- BUG ---\nTitle: One\n================",
		"short closer":       "--- BUG ---\nTitle: One\n---------------",
		"short trailing run": "--- BUG --\nTitle: One\n----------------",
		"empty body":         "--- BUG ---\n\n\t\n----------------",
		"ambiguous":          valid + "\n" + valid,
		"interior separator": "--- BUG ---\nTitle: One\n----------------\nMore details\n----------------",
		"overflow":           "--- BUG ---\n" + strings.Repeat("x", localizationProblemStatementMaxBytes+1) + "\n----------------",
	}
	for name, prompt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := framedLocalizationProblemStatement(prompt)
			require.False(t, ok)
			require.Empty(t, got)
		})
	}
}

func TestFramedLocalizationProblemStatementAllowsExactByteLimit(t *testing.T) {
	statement := strings.Repeat("x", localizationProblemStatementMaxBytes)
	got, ok := framedLocalizationProblemStatement("--- ISSUE ---\n" + statement + "\n----------------")
	require.True(t, ok)
	require.Equal(t, statement, got)
}
