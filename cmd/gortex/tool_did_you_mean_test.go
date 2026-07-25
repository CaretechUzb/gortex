package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestMaybeToolInvocationHint(t *testing.T) {
	// Mirror execute(): cobra's own `help` and `completion` commands are on
	// the tree before the hint ever inspects it.
	primeBuiltinCommands(nil)

	cases := []struct {
		name     string
		args     []string
		wantHint bool
		// substrings that must appear in the emitted hint when wantHint is true
		wantContains []string
	}{
		{
			name:         "read_file tool name",
			args:         []string{"read_file", "foo.go"},
			wantHint:     true,
			wantContains: []string{"gortex call read --arg target='{\"file\":\"<file>\"}'", "legacy Gortex MCP name"},
		},
		{
			name:         "long-tail legacy name maps to public discovery",
			args:         []string{"flow_between"},
			wantHint:     true,
			wantContains: []string{"gortex call capabilities --arg domain='trace' --arg operation='flow'", "legacy Gortex MCP name"},
		},
		{
			name:         "get_symbol_source tool name",
			args:         []string{"get_symbol_source"},
			wantHint:     true,
			wantContains: []string{"gortex call read --arg target='{\"symbol\":\"<file>::<Name|Recv.Name>\"}'"},
		},
		{
			name:         "dedicated facade tool name",
			args:         []string{"read"},
			wantHint:     true,
			wantContains: []string{"gortex call read --arg target='{\"file\":\"<file>\"}'", "gortex call capabilities"},
		},
		{
			name:         "tool name behind leading global flag",
			args:         []string{"--config", "x.yaml", "read_file", "foo.go"},
			wantHint:     true,
			wantContains: []string{"gortex call read --arg target='{\"file\":\"<file>\"}'"},
		},
		{
			name:         "fuzzy: reindex suggests reindex_repository",
			args:         []string{"reindex"},
			wantHint:     true,
			wantContains: []string{`unknown command "reindex"`, "gortex call workspace_admin --arg operation=reindex"},
		},
		{
			name:         "fuzzy: index suggests index_repository",
			args:         []string{"index", "."},
			wantHint:     true,
			wantContains: []string{`unknown command "index"`, "gortex call workspace_admin --arg operation=index"},
		},
		{
			name:         "fuzzy facade uses negotiating command",
			args:         []string{"publish"},
			wantHint:     true,
			wantContains: []string{`unknown command "publish"`, "gortex call publish_review --arg operation='<operation>'"},
		},
		{
			name:     "real cobra command is not intercepted",
			args:     []string{"daemon", "status"},
			wantHint: false,
		},
		{
			name:     "call itself is a real command",
			args:     []string{"call", "read_file"},
			wantHint: false,
		},
		{
			name:     "genuinely unknown non-tool falls through to cobra",
			args:     []string{"florpwidget"},
			wantHint: false,
		},
		{
			name:     "no args",
			args:     nil,
			wantHint: false,
		},
		{
			name:     "help flag is not a tool",
			args:     []string{"--help"},
			wantHint: false,
		},
		// Cobra registers these itself, late. Shadowing `completion` sent
		// `gortex completion zsh` a graph_completion_search suggestion instead
		// of the script, which also broke the completions a packager generates
		// by running the binary at install time.
		{
			name:     "completion command reaches cobra's generator",
			args:     []string{"completion", "zsh"},
			wantHint: false,
		},
		{
			name:     "help command reaches cobra",
			args:     []string{"help"},
			wantHint: false,
		},
		{
			name:     "shell completion protocol command is never hinted",
			args:     []string{cobra.ShellCompRequestCmd, "completion", ""},
			wantHint: false,
		},
		{
			name:     "descriptionless completion protocol command is never hinted",
			args:     []string{cobra.ShellCompNoDescRequestCmd, ""},
			wantHint: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := maybeToolInvocationHint(&buf, tc.args)
			if got != tc.wantHint {
				t.Fatalf("maybeToolInvocationHint(%v) = %v, want %v (output: %q)", tc.args, got, tc.wantHint, buf.String())
			}
			if !tc.wantHint {
				if buf.Len() != 0 {
					t.Errorf("expected no output when no hint fires, got %q", buf.String())
				}
				return
			}
			out := buf.String()
			// Must never emit the invalid bare `gortex <verb>` shape (only the
			// `gortex call <verb>` fallback is valid).
			verb := firstPositionalArg(tc.args)
			if strings.Contains(out, "gortex "+verb+" ") || strings.HasSuffix(strings.TrimSpace(out), "gortex "+verb) {
				t.Errorf("hint emitted the invalid bare shape `gortex %s`:\n%s", verb, out)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("hint missing %q:\n%s", want, out)
				}
			}
			if !strings.Contains(out, "gortex call ") {
				t.Errorf("hint must teach the `gortex call` shape:\n%s", out)
			}
		})
	}
}

// TestPrimeBuiltinCommandsRegistersBuiltins asserts the commands cobra would
// otherwise only add from inside Execute are on the tree beforehand — the
// precondition isKnownRootCommand depends on — and that priming stays
// idempotent, since cobra runs the same initializers again during Execute.
func TestPrimeBuiltinCommandsRegistersBuiltins(t *testing.T) {
	primeBuiltinCommands(nil)

	for _, name := range []string{"completion", "help"} {
		if !isKnownRootCommand(name) {
			t.Errorf("built-in %q is not a known root command after priming", name)
		}
	}

	// The per-shell subcommands must resolve too: these are what a packager
	// runs to generate completion scripts at install time.
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		cmd, _, err := rootCmd.Find([]string{"completion", shell})
		if err != nil {
			t.Errorf("completion %s does not resolve: %v", shell, err)
			continue
		}
		if cmd.Name() != shell {
			t.Errorf("completion %s resolved to %q, want %q", shell, cmd.Name(), shell)
		}
	}

	before := len(rootCmd.Commands())
	primeBuiltinCommands(nil)
	if after := len(rootCmd.Commands()); after != before {
		t.Errorf("primeBuiltinCommands not idempotent: commands %d -> %d", before, after)
	}
}

func TestCompactCLIInvocationQuotesShellPlaceholders(t *testing.T) {
	for _, tool := range []string{"analyze", "ask", "capabilities", "explore", "search", "relations"} {
		if got := cliInvocationForTool(tool); strings.Contains(got, "=<") {
			t.Errorf("cliInvocationForTool(%q) contains shell redirection syntax: %s", tool, got)
		}
	}
}
