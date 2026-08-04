package claudecli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/llm"
)

// fakeClaude writes a tiny shell script that impersonates the
// `claude` CLI: it echoes the script-baked stdout payload, optionally
// captures the args + stdin into sidecar files for assertions, and
// exits with the script-baked exit code. The returned path is the
// absolute path to the script.
//
// A shell-script fake is the smallest portable surface that exercises
// every code path the provider has — arg construction, stdin
// piping, exit-status handling, stderr capture — without pulling in
// the full TestHelperProcess re-exec dance.
func fakeClaude(t *testing.T, dir string, opts fakeOpts) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	script := filepath.Join(dir, "fake-claude.sh")

	argsLog := filepath.Join(dir, "args.txt")
	stdinLog := filepath.Join(dir, "stdin.txt")
	stderrPayload := strings.ReplaceAll(opts.stderr, "'", "'\\''")
	stdoutPayload := strings.ReplaceAll(opts.stdout, "'", "'\\''")

	// NUL-separated so an argument carrying newlines (the JSON-Schema
	// rider does) still round-trips as exactly one argv entry.
	body := "#!/bin/sh\n" +
		"set -e\n" +
		"printf '%s\\0' \"$@\" > '" + argsLog + "'\n" +
		"cat > '" + stdinLog + "'\n"
	if opts.sleep > 0 {
		body += fmt.Sprintf("sleep %d\n", int(opts.sleep.Seconds()+1))
	}
	if stderrPayload != "" {
		body += "printf '%s' '" + stderrPayload + "' >&2\n"
	}
	if stdoutPayload != "" {
		body += "printf '%s' '" + stdoutPayload + "'\n"
	}
	if opts.exitCode != 0 {
		body += fmt.Sprintf("exit %d\n", opts.exitCode)
	}

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	return script
}

type fakeOpts struct {
	stdout   string
	stderr   string
	exitCode int
	sleep    time.Duration
}

func readSidecar(t *testing.T, scriptPath, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(scriptPath), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// argv reconstructs the exact argument vector the fake CLI received.
func argv(t *testing.T, scriptPath string) []string {
	t.Helper()
	raw := readSidecar(t, scriptPath, "args.txt")
	raw = strings.TrimSuffix(raw, "\x00")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\x00")
}

// flagValue returns the argument following flag, and whether flag was
// passed at all (a trailing flag reports "" plus true).
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a != flag {
			continue
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
		return "", true
	}
	return "", false
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestNew_BinaryNotFound(t *testing.T) {
	if _, err := New(llm.ClaudeCLIConfig{Binary: "claude-nonexistent-zzzzz"}); err == nil {
		t.Fatal("expected error when binary is not on PATH")
	}
}

func TestNew_DefaultsBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	// Stash a fake `claude` on PATH so the default binary name resolves.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p, err := New(llm.ClaudeCLIConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if p.Name() != "claudecli" {
		t.Errorf("Name()=%q want claudecli", p.Name())
	}
}

func TestComplete_FreeformSuccess(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "hello world"})

	p, err := New(llm.ClaudeCLIConfig{Binary: script})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "be terse"},
			{Role: llm.RoleUser, Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello world" {
		t.Errorf("text=%q want hello world", resp.Text)
	}

	args := argv(t, script)
	for _, want := range []string{"--print", "--output-format", "text"} {
		if !hasArg(args, want) {
			t.Errorf("args missing %q\nargs=%q", want, args)
		}
	}
	if got, ok := flagValue(args, "--system-prompt"); !ok || got != "be terse" {
		t.Errorf("--system-prompt=%q present=%v, want %q", got, ok, "be terse")
	}
	stdin := readSidecar(t, script, "stdin.txt")
	if !strings.Contains(stdin, "User: hi") {
		t.Errorf("stdin missing user turn:\n%s", stdin)
	}
	if strings.Contains(stdin, "be terse") {
		t.Error("system content must travel via --system-prompt, not stdin")
	}
}

// The provider must REPLACE Claude Code's default system prompt rather
// than append to it: appending leaves the interactive agent persona,
// the discovered CLAUDE.md files and the injected cwd/environment block
// in force, and that context beats the structured-output instruction.
func TestComplete_ReplacesDefaultSystemPrompt(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := argv(t, script)
	if hasArg(args, "--append-system-prompt") {
		t.Errorf("must not append to the default system prompt; args=%q", args)
	}
	// Emitted even with no system messages — an empty replacement is
	// what strips the default agentic prompt.
	got, ok := flagValue(args, "--system-prompt")
	if !ok || got != "" {
		t.Errorf("--system-prompt=%q present=%v, want an empty replacement", got, ok)
	}
}

// The headless defaults: hooks off (the reported failure — a user's
// SessionEnd hook exiting nonzero failed the whole call), no native
// toolset, no MCP servers.
func TestComplete_HeadlessDefaults(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := argv(t, script)

	if got, ok := flagValue(args, "--settings"); !ok || !strings.Contains(got, `"disableAllHooks":true`) {
		t.Errorf("--settings=%q present=%v, want disableAllHooks", got, ok)
	}
	if got, ok := flagValue(args, "--tools"); !ok || got != "" {
		t.Errorf("--tools=%q present=%v, want an empty toolset", got, ok)
	}
	if !hasArg(args, "--strict-mcp-config") {
		t.Errorf("args missing --strict-mcp-config; args=%q", args)
	}
}

// --tools is variadic: it swallows every following argument up to the
// next flag. The value must therefore always be followed by one.
func TestComplete_ToolsFlagIsFollowedByAFlag(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := argv(t, script)
	i := -1
	for n, a := range args {
		if a == "--tools" {
			i = n
			break
		}
	}
	if i < 0 {
		t.Fatalf("no --tools in args=%q", args)
	}
	if i+2 >= len(args) {
		t.Fatalf("--tools value is the last argument; a variadic flag must be followed by another flag: args=%q", args)
	}
	if next := args[i+2]; !strings.HasPrefix(next, "-") {
		t.Errorf("argument after --tools value is %q, want a flag (it would be swallowed); args=%q", next, args)
	}
}

// Any flag in llm.claudecli.args wins over the matching headless
// default — the config file stays the final word.
func TestComplete_ArgsOverrideHeadlessDefaults(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{
		Binary: script,
		Args:   []string{"--tools", "default", "--settings=/etc/claude.json"},
	})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := argv(t, script)

	var tools, settings int
	for _, a := range args {
		switch {
		case a == "--tools":
			tools++
		case a == "--settings" || strings.HasPrefix(a, "--settings="):
			settings++
		}
	}
	if tools != 1 {
		t.Errorf("--tools appears %d times, want 1 (the user's); args=%q", tools, args)
	}
	if settings != 1 {
		t.Errorf("--settings appears %d times, want 1 (the user's); args=%q", settings, args)
	}
	if got, _ := flagValue(args, "--tools"); got != "default" {
		t.Errorf("--tools=%q, want the user's %q", got, "default")
	}
	// Untouched defaults still apply.
	if !hasArg(args, "--strict-mcp-config") {
		t.Errorf("args missing --strict-mcp-config; args=%q", args)
	}
}

// A user-supplied --system-prompt replaces the base prompt, so the
// provider's own instruction (which carries the JSON-Schema rider) must
// survive as an append rather than be dropped.
func TestComplete_UserSystemPromptKeepsSchemaRider(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: `{"terms":["a"]}`})

	p, _ := New(llm.ClaudeCLIConfig{
		Binary: script,
		Args:   []string{"--system-prompt", "you are terse"},
	})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "expand 'x'"}},
		Shape:    llm.ShapeExpandTerms,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	args := argv(t, script)

	if got, ok := flagValue(args, "--system-prompt"); !ok || got != "you are terse" {
		t.Errorf("--system-prompt=%q present=%v, want the user's", got, ok)
	}
	appended, ok := flagValue(args, "--append-system-prompt")
	if !ok {
		t.Fatalf("schema rider dropped; args=%q", args)
	}
	if !strings.Contains(appended, "JSON Schema") {
		t.Errorf("--append-system-prompt=%q, want the JSON-Schema rider", appended)
	}
}

func TestComplete_PassesModel(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script, Model: "sonnet"})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, ok := flagValue(argv(t, script), "--model"); !ok || got != "sonnet" {
		t.Errorf("--model=%q present=%v, want sonnet", got, ok)
	}
}

func TestComplete_StructuredExtractsJSON(t *testing.T) {
	wrapped := "Sure, here you go:\n```json\n{\"terms\":[\"bcrypt\",\"argon2\"]}\n```\n"
	script := fakeClaude(t, "", fakeOpts{stdout: wrapped})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "expand 'password hashing'"}},
		Shape:    llm.ShapeExpandTerms,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != `{"terms":["bcrypt","argon2"]}` {
		t.Errorf("text=%q want the unwrapped JSON object", resp.Text)
	}

	rider, ok := flagValue(argv(t, script), "--system-prompt")
	if !ok || !strings.Contains(rider, "JSON Schema") {
		t.Errorf("structured request must inject a JSON Schema rider; got %q", rider)
	}
}

func TestComplete_StructuredNoJSONErrors(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "I cannot help with that."})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}},
		Shape:    llm.ShapeExpandTerms,
	}); err == nil {
		t.Fatal("expected error when structured response carried no JSON")
	}
}

func TestComplete_NonZeroExit(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{exitCode: 2, stderr: "auth required"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "auth required") {
		t.Errorf("error should include stderr snippet; got: %v", err)
	}
}

// `claude -p` prints its terminal errors on stdout and leaves stderr
// empty, so a stderr-only error message collapses a real diagnosis into
// an opaque "exit status 1".
func TestComplete_NonZeroExitFallsBackToStdout(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{exitCode: 1, stdout: "Error: Reached max turns (1)"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	_, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "Reached max turns") {
		t.Errorf("error should fall back to the stdout snippet; got: %v", err)
	}
}

func TestComplete_EmptyResponseErrors(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: ""})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestComplete_ContextCancellation(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "late", sleep: 2 * time.Second})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script, TimeoutSeconds: 1})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestComplete_ExtraArgsForwarded(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "ok"})

	p, _ := New(llm.ClaudeCLIConfig{Binary: script, Args: []string{"--permission-mode", "plan"}})
	defer p.Close()

	if _, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, ok := flagValue(argv(t, script), "--permission-mode"); !ok || got != "plan" {
		t.Errorf("--permission-mode=%q present=%v, want plan", got, ok)
	}
}

func TestFlatten_Roles(t *testing.T) {
	sys, prompt := flatten([]llm.Message{
		{Role: llm.RoleSystem, Content: "rule 1"},
		{Role: llm.RoleSystem, Content: "rule 2"},
		{Role: llm.RoleUser, Content: "q1"},
		{Role: llm.RoleAssistant, Content: "a1"},
		{Role: llm.RoleTool, Content: "[1,2,3]", ToolName: "search_symbols"},
		{Role: llm.RoleUser, Content: "q2"},
	})
	if sys != "rule 1\n\nrule 2" {
		t.Errorf("system=%q", sys)
	}
	for _, want := range []string{"User: q1", "Assistant: a1", "Tool result (search_symbols)", "User: q2"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nprompt=\n%s", want, prompt)
		}
	}
}

// Sanity check: the test helper itself can run a child process. If
// this fails the environment doesn't support `/bin/sh` and every
// other subprocess test in this file will skip cleanly.
func TestHelper_FakeScriptIsExecutable(t *testing.T) {
	script := fakeClaude(t, "", fakeOpts{stdout: "alive"})
	cmd := exec.Command(script, "ping")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	if err != nil {
		// On a CI runner without /bin/sh, surface as skip instead of
		// fail so we don't paint subprocess CI red.
		t.Skipf("cannot exec fake script: %v", err)
	}
	if !strings.Contains(string(out), "alive") {
		t.Errorf("output=%q", out)
	}
}
