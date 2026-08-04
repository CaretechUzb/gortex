// Package agentcli is the shared engine behind the simple coding-agent
// CLI subprocess providers — GitHub Copilot (`copilot`), Cursor
// (`cursor-agent`), and opencode (`opencode`). Each shells out to a
// locally installed, already-signed-in binary, reusing the user's
// existing subscription instead of an API key in gortex's environment
// (the same model as the `claudecli` and `codex` providers).
//
// The three differ only in their binary, their model flag, and how the
// prompt is delivered (a `-p` flag, a trailing positional, or stdin).
// Spec captures those differences; the spawn/timeout/capture/extract
// machinery is shared here. Each provider package is a thin
// constructor that fills in a Spec.
//
// Structured output reuses the CLI plumbing shared with claudecli/codex:
// a JSON-Schema rider on the prompt (llm.AppendSchemaInstruction) plus
// llm.ExtractJSON on the response — none of these CLIs has a native
// structured-output mechanism.
package agentcli

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

// DefaultTimeout caps one Complete call when the Spec leaves Timeout
// zero. CLI startup plus a model round-trip runs longer than a bare
// HTTP call, so the default is generous.
const DefaultTimeout = 180 * time.Second

// PromptDelivery selects how the flattened prompt reaches the CLI.
type PromptDelivery int

const (
	// DeliveryFlag passes the prompt as the value of PromptFlag (e.g.
	// `copilot -p "<prompt>"`).
	DeliveryFlag PromptDelivery = iota
	// DeliveryArg appends the prompt as a trailing positional argument
	// (e.g. `opencode run "<prompt>"`).
	DeliveryArg
	// DeliveryStdin pipes the prompt on stdin. Args may include a `-`
	// sentinel telling the CLI to read stdin.
	DeliveryStdin
)

// Spec describes one coding-agent CLI to the shared engine.
type Spec struct {
	// ProviderID is the llm.Provider Name() — "copilot" / "cursor" /
	// "opencode". Drives the prompt tier and diagnostics.
	ProviderID string
	// Binary is the resolved executable path (post-LookPath).
	Binary string
	// Model is the model slug; passed with ModelFlag when both are set.
	Model string
	// ModelFlag is the flag that selects the model (e.g. "--model").
	// Empty means the CLI is left on its own default.
	ModelFlag string
	// BaseArgs are the leading subcommand/flags before model + prompt
	// (e.g. ["run"] for opencode, ["--output-format", "text"] for
	// cursor).
	BaseArgs []string
	// ExtraArgs are user-supplied arguments appended after BaseArgs and
	// the model flag, before the prompt.
	ExtraArgs []string
	// Delivery selects how the prompt is handed to the CLI.
	Delivery PromptDelivery
	// PromptFlag is the flag name used when Delivery == DeliveryFlag.
	PromptFlag string
	// Timeout caps one Complete call. Zero → DefaultTimeout.
	Timeout time.Duration
}

// Provider implements llm.Provider over one coding-agent CLI.
type Provider struct {
	spec Spec
}

var _ llm.Provider = (*Provider)(nil)

// New resolves the configured binary on $PATH (so misconfiguration
// surfaces at startup, not on the first Complete) and returns a
// Provider. binary is the configured name/path; fallback is the
// canonical default when binary is empty.
func New(spec Spec, binary, fallback string) (llm.Provider, error) {
	bin := strings.TrimSpace(binary)
	if bin == "" {
		bin = fallback
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%s: binary %q not found on PATH: %w", spec.ProviderID, bin, err)
	}
	spec.Binary = resolved
	if spec.Timeout <= 0 {
		spec.Timeout = DefaultTimeout
	}
	return &Provider{spec: spec}, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.spec.ProviderID }

// Close is a no-op — every Complete spawns a fresh subprocess; there is
// no long-lived connection to release.
func (p *Provider) Close() error { return nil }

// Complete implements llm.Provider. It flattens the conversation into a
// single prompt (system turns folded into a leading block), spawns one
// subprocess, captures stdout, and — for structured shapes — appends a
// JSON-Schema rider and extracts the JSON from a possibly chatty reply.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	prompt := FlattenPrompt(req.Messages)
	structured := req.Shape != llm.ShapeFreeform
	if structured {
		prompt = llm.AppendSchemaInstruction(prompt, req.Shape, req.Tools)
	}
	if strings.TrimSpace(prompt) == "" {
		return llm.CompletionResponse{}, fmt.Errorf("%s: empty prompt", p.spec.ProviderID)
	}

	args := append([]string(nil), p.spec.BaseArgs...)
	if p.spec.ModelFlag != "" && p.spec.Model != "" {
		args = append(args, p.spec.ModelFlag, p.spec.Model)
	}
	args = append(args, p.spec.ExtraArgs...)

	var stdin *strings.Reader
	switch p.spec.Delivery {
	case DeliveryFlag:
		flag := p.spec.PromptFlag
		if flag == "" {
			flag = "-p"
		}
		args = append(args, flag, prompt)
	case DeliveryArg:
		args = append(args, prompt)
	case DeliveryStdin:
		stdin = strings.NewReader(prompt)
	}

	runCtx := ctx
	if p.spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, p.spec.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, p.spec.Binary, args...)
	platform.ConfigureBackgroundCommand(cmd)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return llm.CompletionResponse{}, fmt.Errorf("%s: timed out after %s: %s", p.spec.ProviderID, p.spec.Timeout, llm.FailureSnippet(stdout.Bytes(), stderr.Bytes()))
		}
		if msg := llm.FailureSnippet(stdout.Bytes(), stderr.Bytes()); msg != "" {
			return llm.CompletionResponse{}, fmt.Errorf("%s: %w: %s", p.spec.ProviderID, err, msg)
		}
		return llm.CompletionResponse{}, fmt.Errorf("%s: %w", p.spec.ProviderID, err)
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return llm.CompletionResponse{}, fmt.Errorf("%s: empty response from CLI", p.spec.ProviderID)
	}
	if structured {
		extracted, ok := llm.ExtractJSON(text)
		if !ok {
			return llm.CompletionResponse{}, fmt.Errorf("%s: response carried no JSON: %s", p.spec.ProviderID, llm.Snippet([]byte(text)))
		}
		text = extracted
	}
	return llm.CompletionResponse{Text: text}, nil
}

// FlattenPrompt collapses the provider-neutral conversation into a
// single prompt string. None of these CLIs has a system-prompt flag,
// so RoleSystem turns are folded into a leading "System instructions:"
// block; the rest render as "User:" / "Assistant:" / "Tool result:"
// turns.
func FlattenPrompt(in []llm.Message) string {
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
