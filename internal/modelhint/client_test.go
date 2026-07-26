package modelhint

import "testing"

// The hint is keyed by working directory, so on a machine where two agents
// share a repo, whichever agent reads it gets whatever the last one wrote.
// These tests pin the guard that keeps a Codex call from being booked against
// a Claude model — a mistake that does not merely lose attribution, it prices
// the call at another vendor's rates.

func TestNormalizeClientBridgesTheTwoSpellings(t *testing.T) {
	cases := map[string]string{
		"codex-mcp-client": "codex",
		"codex":            "codex",
		"CODEX-MCP-CLIENT": "codex",
		"  codex  ":        "codex",
		"claude-code":      "claude-code",
		"kimi-mcp":         "kimi",
		"":                 "",
	}
	for in, want := range cases {
		if got := NormalizeClient(in); got != want {
			t.Errorf("NormalizeClient(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameClient(t *testing.T) {
	same := [][2]string{
		{"codex", "codex-mcp-client"}, // the hook's name vs the MCP client's
		{"claude-code", "claude-code"},
		{"", "codex-mcp-client"}, // unknown cannot contradict
		{"claude-code", ""},
	}
	for _, pair := range same {
		if !SameClient(pair[0], pair[1]) {
			t.Errorf("SameClient(%q, %q) = false, want true", pair[0], pair[1])
		}
	}

	// The case that produced 3,224 mis-attributed calls: a Claude-written
	// hint read by a Codex session working in the same repo.
	if SameClient("claude-code", "codex-mcp-client") {
		t.Error("a Claude hint must not be adopted by a Codex session")
	}
	if SameClient("codex", "claude-code") {
		t.Error("a Codex hint must not be adopted by a Claude session")
	}
}

// TestReadWriteRoundTripCarriesClient: the guard is only as good as the
// client actually being recorded on the hint.
func TestReadWriteRoundTripCarriesClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dirEnvVar, dir)

	Write("/repo", "gpt-5.6-sol", "codex")
	got, ok := Read("/repo")
	if !ok {
		t.Fatal("hint not readable")
	}
	if got.Model != "gpt-5.6-sol" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Client != "codex" {
		t.Errorf("client = %q — without it the guard cannot tell agents apart", got.Client)
	}
	if !SameClient(got.Client, "codex-mcp-client") {
		t.Error("the recorded client should match the MCP client that will read it")
	}
	if SameClient(got.Client, "claude-code") {
		t.Error("the recorded client should not match a different agent")
	}
}
