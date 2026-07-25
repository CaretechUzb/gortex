package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/toolref"
)

// maybeToolInvocationHint intercepts the `gortex <tool>` misuse: an agent that
// saw a bare MCP tool name and tried to run it as a top-level verb (e.g.
// `gortex read_file foo.go`). There is no such verb: every tool uses `gortex
// call <tool>`, which selects the correct legacy or compact surface for that
// connection. Cobra would otherwise print a bare "unknown command" with no
// recovery path.
//
// Cheap and daemon-free: the fast rejects (flag, known verb) run first, so the
// tool registry is only consulted for an argument that is otherwise an unknown
// command — never on a normal invocation.
func maybeToolInvocationHint(w io.Writer, args []string) bool {
	verb := firstPositionalArg(args)
	if verb == "" || strings.HasPrefix(verb, "-") {
		return false
	}
	if isKnownRootCommand(verb) {
		return false // a real cobra verb — let cobra route it
	}
	if gortexmcp.IsFacadeToolName(verb) {
		fmt.Fprintf(w, "gortex: %q is a compact Gortex MCP tool, not a bare command.\n", verb)
		fmt.Fprintf(w, "Run it from a shell with:\n  %s\n", cliInvocationForTool(verb))
		fmt.Fprintln(w, "The argument object is the same as the compact MCP schema; use `gortex call capabilities --arg domain=<tool>` to discover operations.")
		return true
	}
	if gortexmcp.IsRegisteredToolName(verb) {
		fmt.Fprintf(w, "gortex: %q is a legacy Gortex MCP name, not a bare command.\n", verb)
		fmt.Fprintf(w, "Use the public operation, or discover its exact schema first:\n  %s\n", cliInvocationForTool(verb))
		return true
	}

	// Not an exact tool name — try a conservative fuzzy match. Agents invent
	// truncations of the real tool names (`gortex index`, `gortex reindex`), so
	// a prefix / whole-token match against the registry converts those from a
	// dead end into recovery too. No match — let cobra's error stand.
	candidates := fuzzyToolCandidates(verb)
	if len(candidates) == 0 {
		return false
	}
	fmt.Fprintf(w, "gortex: unknown command %q. The closest Gortex MCP tools:\n", verb)
	for _, c := range candidates {
		fmt.Fprintf(w, "  %s\n", cliInvocationForTool(c))
	}
	fmt.Fprintln(w, "General form: gortex call <tool> --arg k=v  (there is no bare `gortex <tool>` verb).")
	return true
}

func cliInvocationForTool(tool string) string {
	if gortexmcp.IsFacadeToolName(tool) {
		switch tool {
		case "analyze":
			return "gortex call analyze --arg kind='<kind>'"
		case "ask":
			return "gortex call ask --arg question='<question>'"
		case "capabilities":
			return "gortex call capabilities --arg domain='<tool>'"
		case "explore":
			return "gortex call explore --arg task='<request>'"
		case "read":
			return `gortex call read --arg target='{"file":"<file>"}'`
		case "search":
			return "gortex call search --arg operation=symbols --arg query='<name>'"
		default:
			return "gortex call " + tool + " --arg operation='<operation>'"
		}
	}
	if example, ok := toolref.ConcreteCLIFallback(tool); ok {
		return example
	}
	if domain, operation, ok := gortexmcp.PublicOperationForLegacy(tool); ok {
		return "gortex call capabilities --arg domain='" + domain + "' --arg operation='" + operation + "' --arg detail=schema"
	}
	return toolref.CLIFallback(tool)
}

// fuzzyMinVerbLen gates the fuzzy tool match: shorter fragments are too
// ambiguous to suggest anything for, so they fall through to cobra.
const fuzzyMinVerbLen = 4

// fuzzyToolCandidates returns up to two registered tool names the unknown verb
// plausibly meant, ranked prefix matches first (`reindex` → reindex_repository)
// then whole-token matches (`usages` → find_usages), alphabetical within a
// rank. Deliberately conservative — a prefix must land on an underscore
// boundary and a token must match exactly, so an unrelated verb never draws a
// suggestion and cobra's own error remains the fallback.
func fuzzyToolCandidates(verb string) []string {
	if len(verb) < fuzzyMinVerbLen {
		return nil
	}
	v := strings.ToLower(verb)
	var prefix, token []string
	for _, name := range gortexmcp.RegisteredToolNames() {
		switch {
		case strings.HasPrefix(name, v+"_"):
			prefix = append(prefix, name)
		case tokenOf(name, v):
			token = append(token, name)
		}
	}
	out := prefix
	out = append(out, token...)
	if len(out) > 2 {
		out = out[:2]
	}
	return out
}

// tokenOf reports whether v equals one of name's underscore-delimited tokens.
func tokenOf(name, v string) bool {
	for _, t := range strings.Split(name, "_") {
		if t == v {
			return true
		}
	}
	return false
}

// firstPositionalArg returns the first argument that is not an option flag,
// skipping the two value-taking persistent flags in their space-separated form
// so `gortex --config x read_file` still resolves to the intended verb. Stops
// at a "--" terminator.
func firstPositionalArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "-") {
			if a == "--config" || a == "--log-level" {
				i++ // skip the flag's space-separated value
			}
			continue
		}
		return a
	}
	return ""
}

// primeBuiltinCommands registers the two commands cobra otherwise only adds
// from inside Execute: `help` and `completion` (with its per-shell
// subcommands). Until they exist the command tree is incomplete, so
// isKnownRootCommand — which decides whether a verb is real by walking that
// tree — reports a built-in as unknown and the tool hint shadows it. That is
// how `gortex completion zsh` came to answer with a `graph_completion_search`
// suggestion instead of the script, which in turn broke the completions
// Homebrew generates by running the binary at install time.
//
// Both initializers are idempotent, so cobra's own calls during Execute become
// no-ops. args is the command line below the binary name — cobra reads it to
// decide whether a root whose only subcommand would be `completion` is
// actually being asked for it, and it is threaded through a parameter rather
// than read from os.Args so tests can drive it.
func primeBuiltinCommands(args []string) {
	rootCmd.InitDefaultHelpCmd()
	rootCmd.InitDefaultCompletionCmd(args...)
}

// isKnownRootCommand reports whether name matches a registered top-level cobra
// command or one of its aliases. No daemon, no tool registry — a plain walk of
// the command tree, which primeBuiltinCommands has topped up with cobra's
// lazily-added built-ins.
func isKnownRootCommand(name string) bool {
	// Cobra adds the shell-completion protocol commands from an unexported
	// initializer that priming cannot reach, so they never appear in the walk
	// below. They carry every completion request a generated script makes, and
	// a hint printed in their place would land in the shell's candidate list.
	if name == cobra.ShellCompRequestCmd || name == cobra.ShellCompNoDescRequestCmd {
		return true
	}
	for _, c := range rootCmd.Commands() {
		if c.Name() == name {
			return true
		}
		for _, a := range c.Aliases {
			if a == name {
				return true
			}
		}
	}
	return false
}
