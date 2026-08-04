// Package claudecli is the Claude Code CLI llm.Provider.
//
// It is pure Go — available in every build, no `-tags llama` needed.
// Inference is delegated to the user's locally installed `claude`
// binary, which reuses the user's Claude Code subscription instead of
// requiring an Anthropic API key. Each Complete call spawns one
// `claude -p` subprocess: the conversation is flattened to text, the
// system prompt is forwarded via --system-prompt, and the prompt text
// is fed on stdin so very large contexts don't trip ARG_MAX.
//
// The spawn is deliberately stripped down to a completion engine —
// see headlessArgs for why an interactive Claude Code session's
// hooks, native tools, MCP servers and default system prompt all have
// to be turned off before a one-shot call is reliable.
//
// Structured output (the expand / rerank / verify shapes and the
// agent tool-call shape) is obtained by appending a JSON-Schema
// instruction to the system prompt and parsing the first valid JSON
// object out of the response — the CLI has no native structured-
// output mechanism. That schema-rider + JSON-extraction logic is
// shared with the `codex` provider; it lives in llm.AppendSchema-
// Instruction / llm.ExtractJSON. The agent tool-loop itself uses the
// *emulated* protocol: tool calls and results travel as plain text
// turns, so a single llm.Message shape works across all providers.
package claudecli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/platform"
)

// defaultTimeout caps one Complete call when the user hasn't set
// claudecli.timeout_seconds in config. Claude Code CLI startup plus
// one model round-trip is comfortably under 120s for the small
// prompts the assist/agent loop emits.
const defaultTimeout = 120 * time.Second

// headlessSettings is the --settings payload the provider passes by
// default. A one-shot completion has no session lifecycle worth
// hooking, but the spawn still loads the user's interactive Claude
// Code configuration — so a SessionEnd hook that fails in a headless
// context (or a plugin hook that is simply meaningless there) exits
// the CLI nonzero and takes the whole Complete call down with it.
const headlessSettings = `{"disableAllHooks":true}`

// Provider implements llm.Provider against the `claude` CLI.
type Provider struct {
	binary  string
	model   string
	extra   []string
	timeout time.Duration
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the Claude CLI provider. It verifies the binary is
// reachable on $PATH (or as an absolute path) so misconfiguration
// surfaces at startup, not on the first Complete call.
func New(cfg llm.ClaudeCLIConfig) (llm.Provider, error) {
	bin := strings.TrimSpace(cfg.Binary)
	if bin == "" {
		bin = "claude"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("claudecli: binary %q not found on PATH: %w", bin, err)
	}
	timeout := defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return &Provider{
		binary:  resolved,
		model:   strings.TrimSpace(cfg.Model),
		extra:   append([]string(nil), cfg.Args...),
		timeout: timeout,
	}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "claudecli" }

// Close is a no-op — every Complete spawns a fresh subprocess; there
// is no long-lived connection or model handle to release.
func (p *Provider) Close() error { return nil }

// Complete implements llm.Provider. It runs one `claude -p`
// subprocess: the system messages are joined and forwarded via
// --system-prompt, every other message is flattened into a chat-style
// prompt that is piped on stdin, and stdout is captured as the
// model's text. For structured shapes the schema is injected into the
// system prompt and the first balanced JSON object is extracted from
// the response.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	system, prompt := flatten(req.Messages)
	structured := req.Shape != llm.ShapeFreeform
	if structured {
		system = llm.AppendSchemaInstruction(system, req.Shape, req.Tools)
	}

	args := []string{"--print", "--output-format", "text"}
	args = append(args, p.headlessArgs()...)
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	// --max-turns pins the agent loop inside Claude Code to a single
	// turn — every llm.Provider caller assumes one single-shot
	// response. The per-response token cap (req.MaxTokens) is
	// best-effort: the CLI exposes no equivalent flag, so we lean on
	// the model's own behaviour given a short system prompt.
	args = append(args, "--max-turns", "1")
	// --system-prompt REPLACES Claude Code's default agentic system
	// prompt; --append-system-prompt only adds to it. Appending leaves
	// the interactive-agent persona, the discovered CLAUDE.md files and
	// the injected environment block ("you are in <cwd>, which is not a
	// git repository") in force, and that context routinely beats the
	// structured-output instruction — the reply comes back as a
	// clarifying question instead of JSON. Replace it, even with an
	// empty string, so this spawn is a plain completion engine.
	if hasFlag(p.extra, "--system-prompt") {
		// The user is supplying their own base prompt. Ours carries the
		// structured-output rider, so demote it to an append rather than
		// let two --system-prompt values fight.
		if system != "" {
			args = append(args, "--append-system-prompt", system)
		}
	} else {
		args = append(args, "--system-prompt", system)
	}
	args = append(args, p.extra...)

	runCtx := ctx
	if p.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, p.binary, args...)
	platform.ConfigureBackgroundCommand(cmd)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Distinguish a context-timeout from an exec failure so the
		// agent loop can log something meaningful.
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return llm.CompletionResponse{}, fmt.Errorf("claudecli: timed out after %s: %s", p.timeout, llm.FailureSnippet(stdout.Bytes(), stderr.Bytes()))
		}
		if msg := llm.FailureSnippet(stdout.Bytes(), stderr.Bytes()); msg != "" {
			return llm.CompletionResponse{}, fmt.Errorf("claudecli: %w: %s", err, msg)
		}
		return llm.CompletionResponse{}, fmt.Errorf("claudecli: %w", err)
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return llm.CompletionResponse{}, errors.New("claudecli: empty response from CLI")
	}
	if structured {
		extracted, ok := llm.ExtractJSON(text)
		if !ok {
			return llm.CompletionResponse{}, fmt.Errorf("claudecli: response carried no JSON: %s", llm.Snippet([]byte(text)))
		}
		text = extracted
	}
	return llm.CompletionResponse{Text: text}, nil
}

// headlessArgs returns the flags that turn an interactive coding agent
// into a one-shot completion engine. Each one is skipped when
// llm.claudecli.args already carries the same flag, so the config file
// stays the final word (`args: ["--tools", "default"]` restores the
// built-in toolset, for instance).
//
// Callers must keep at least one further flag after these — --max-turns
// is unconditional, so that holds — because --tools is variadic: it
// swallows every following argument until the next flag.
func (p *Provider) headlessArgs() []string {
	var args []string
	if !hasFlag(p.extra, "--tools") {
		// Empty toolset. Even under --print the CLI exposes Bash / Read /
		// Grep / …, and a model that opens with a native tool call spends
		// the single --max-turns turn on it: the run then dies with
		// "Reached max turns (1)" and a nonzero exit. --allowed-tools is
		// not a substitute — it governs auto-approval, not availability,
		// so the call is still attempted.
		args = append(args, "--tools", "")
	}
	if !hasFlag(p.extra, "--strict-mcp-config") {
		// Skip the user's MCP servers: startup latency and more tools to
		// be tempted by, for a spawn that only has to emit text.
		args = append(args, "--strict-mcp-config")
	}
	if !hasFlag(p.extra, "--settings") {
		args = append(args, "--settings", headlessSettings)
	}
	return args
}

// hasFlag reports whether argv already carries the named flag, in
// either "--flag value" or "--flag=value" form.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// flatten splits the conversation into a system block (every
// RoleSystem message joined with a blank line) and a chat-style
// prompt (every other message rendered as "User:" / "Assistant:" /
// "Tool result:" turns). The CLI takes the system part via
// --system-prompt and reads the prompt part from stdin. Using stdin
// avoids the ARG_MAX ceiling on long contexts.
func flatten(in []llm.Message) (system, prompt string) {
	var sys []string
	var b strings.Builder
	turns := 0
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				sys = append(sys, s)
			}
		case llm.RoleAssistant:
			if turns > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("Assistant: ")
			b.WriteString(m.Content)
			turns++
		case llm.RoleTool:
			if turns > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(renderToolResult(m))
			turns++
		default:
			if turns > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("User: ")
			b.WriteString(m.Content)
			turns++
		}
	}
	return strings.Join(sys, "\n\n"), b.String()
}

func renderToolResult(m llm.Message) string {
	if m.ToolName != "" {
		return "Tool result (" + m.ToolName + "):\n" + m.Content
	}
	return "Tool result:\n" + m.Content
}
