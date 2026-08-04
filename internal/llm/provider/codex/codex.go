// Package codex is the OpenAI Codex CLI llm.Provider.
//
// It is the `claudecli` analog for OpenAI's Codex: pure Go, available
// in every build (no `-tags llama`), inference delegated to the user's
// locally installed `codex` binary. The binary reuses the user's
// existing Codex / ChatGPT sign-in instead of requiring an
// OPENAI_API_KEY in gortex's environment.
//
// Each Complete call spawns one `codex exec` subprocess in
// non-interactive mode. The conversation is flattened to a single
// prompt fed on stdin (so very large contexts don't trip ARG_MAX);
// Codex has no system-prompt flag, so RoleSystem turns are folded into
// the prompt as a leading "System instructions:" block. The agent's
// final message is captured via `--output-last-message` into a temp
// file — that isolates the answer from Codex's own progress logging
// on stdout. The subprocess runs `--sandbox read-only` so a completion
// can never mutate the working tree.
//
// Structured output (the expand / rerank / verify shapes and the
// agent tool-call shape) reuses the shared CLI plumbing — a JSON-
// Schema rider on the prompt (llm.AppendSchemaInstruction) plus
// llm.ExtractJSON on the response. Codex has no native structured-
// output mechanism, exactly like the `claude` CLI.
package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/platform"
)

// defaultTimeout caps one Complete call when the user hasn't set
// codex.timeout_seconds. Codex CLI startup plus one model round-trip
// runs longer than the `claude` CLI, so the default is more generous.
const defaultTimeout = 180 * time.Second

// Provider implements llm.Provider against the `codex` CLI.
type Provider struct {
	binary  string
	model   string
	extra   []string
	timeout time.Duration
}

var _ llm.Provider = (*Provider)(nil)

// New constructs the Codex CLI provider. It verifies the binary is
// reachable on $PATH (or as an absolute path) so misconfiguration
// surfaces at startup, not on the first Complete call.
func New(cfg llm.CodexConfig) (llm.Provider, error) {
	bin := strings.TrimSpace(cfg.Binary)
	if bin == "" {
		bin = "codex"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("codex: binary %q not found on PATH: %w", bin, err)
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
func (p *Provider) Name() string { return "codex" }

// Close is a no-op — every Complete spawns a fresh subprocess; there
// is no long-lived connection or model handle to release.
func (p *Provider) Close() error { return nil }

// Complete implements llm.Provider. It runs one `codex exec`
// subprocess: the conversation (system turns folded in) is piped on
// stdin, the agent's final message is captured from the
// --output-last-message sidecar, and structured shapes get the
// JSON-Schema rider + extraction treatment.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	prompt := buildPrompt(req.Messages)
	structured := req.Shape != llm.ShapeFreeform
	if structured {
		prompt = llm.AppendSchemaInstruction(prompt, req.Shape, req.Tools)
	}
	if strings.TrimSpace(prompt) == "" {
		return llm.CompletionResponse{}, errors.New("codex: empty prompt")
	}

	// --output-last-message writes ONLY the agent's final message to a
	// sidecar file, isolating the answer from Codex's progress logging
	// on stdout.
	sidecar, err := os.CreateTemp("", "gortex-codex-*.txt")
	if err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("codex: create output sidecar: %w", err)
	}
	sidecarPath := sidecar.Name()
	_ = sidecar.Close()
	defer func() { _ = os.Remove(sidecarPath) }()

	// `codex exec` runs non-interactively (no approval prompts).
	// --sandbox read-only keeps a completion from mutating the tree;
	// --skip-git-repo-check lets it run outside a git repo; the `-`
	// positional makes Codex read the prompt from stdin, dodging
	// ARG_MAX on long contexts.
	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--color", "never",
		"--sandbox", "read-only",
		"--output-last-message", sidecarPath,
	}
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	args = append(args, p.extra...)
	args = append(args, "-")

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
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return llm.CompletionResponse{}, fmt.Errorf("codex: timed out after %s: %s", p.timeout, llm.FailureSnippet(stdout.Bytes(), stderr.Bytes()))
		}
		if msg := llm.FailureSnippet(stdout.Bytes(), stderr.Bytes()); msg != "" {
			return llm.CompletionResponse{}, fmt.Errorf("codex: %w: %s", err, msg)
		}
		return llm.CompletionResponse{}, fmt.Errorf("codex: %w", err)
	}

	text := readResponse(sidecarPath, stdout.String())
	if text == "" {
		return llm.CompletionResponse{}, errors.New("codex: empty response from CLI")
	}
	if structured {
		extracted, ok := llm.ExtractJSON(text)
		if !ok {
			return llm.CompletionResponse{}, fmt.Errorf("codex: response carried no JSON: %s", llm.Snippet([]byte(text)))
		}
		text = extracted
	}
	return llm.CompletionResponse{Text: text}, nil
}

// readResponse prefers the --output-last-message sidecar (the agent's
// final message, clean of progress logs) and falls back to raw stdout
// when the sidecar is missing or empty — e.g. an older Codex build
// that does not honour the flag.
func readResponse(sidecarPath, stdout string) string {
	if raw, err := os.ReadFile(sidecarPath); err == nil {
		if s := strings.TrimSpace(string(raw)); s != "" {
			return s
		}
	}
	return strings.TrimSpace(stdout)
}

// buildPrompt flattens the conversation into a single prompt string.
// Codex has no system-prompt flag, so RoleSystem turns are folded in
// as a leading "System instructions:" block; the rest render as
// "User:" / "Assistant:" / "Tool result:" turns. The whole thing is
// piped on stdin.
func buildPrompt(in []llm.Message) string {
	var sys []string
	var turns strings.Builder
	n := 0
	for _, m := range in {
		switch m.Role {
		case llm.RoleSystem:
			if s := strings.TrimSpace(m.Content); s != "" {
				sys = append(sys, s)
			}
		case llm.RoleAssistant:
			if n > 0 {
				turns.WriteString("\n\n")
			}
			turns.WriteString("Assistant: ")
			turns.WriteString(m.Content)
			n++
		case llm.RoleTool:
			if n > 0 {
				turns.WriteString("\n\n")
			}
			turns.WriteString(renderToolResult(m))
			n++
		default:
			if n > 0 {
				turns.WriteString("\n\n")
			}
			turns.WriteString("User: ")
			turns.WriteString(m.Content)
			n++
		}
	}

	var b strings.Builder
	if len(sys) > 0 {
		b.WriteString("System instructions:\n")
		b.WriteString(strings.Join(sys, "\n\n"))
		if turns.Len() > 0 {
			b.WriteString("\n\n")
		}
	}
	b.WriteString(turns.String())
	return b.String()
}

func renderToolResult(m llm.Message) string {
	if m.ToolName != "" {
		return "Tool result (" + m.ToolName + "):\n" + m.Content
	}
	return "Tool result:\n" + m.Content
}
