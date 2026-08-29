package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// plugin_node_test.go drives the rendered bridge inside a real JavaScript
// runtime against a stub `gortex hook`.
//
// The string assertions in plugin_test.go can only prove the file *says*
// it fails open. This proves it: a stub that exits non-zero, prints
// garbage, or prints nothing has to leave the tool call untouched, and
// only running the code shows that. The alternative — shipping a bridge
// whose failure mode is "the user's session breaks" — is the one outcome
// that must never reach a machine.
//
// The stub is a POSIX shell script, so this is Unix-only; the rendered
// JavaScript is platform-independent and plugin_test.go's `node --check`
// covers syntax everywhere.

// stubHook is the fake `gortex hook`: it records the envelope it was
// given and answers according to $GORTEX_STUB_MODE.
const stubHook = `#!/bin/sh
cat >> "$GORTEX_STUB_CAPTURE"
printf '\n' >> "$GORTEX_STUB_CAPTURE"
case "$GORTEX_STUB_MODE" in
  block)   printf '%s' '{"block":true,"reason":"[Gortex] call search_symbols instead"}' ;;
  tip)     printf '%s' '{"additional_context":"GRAPH TIP"}' ;;
  orient)  printf '%s' '{"orientation":"BRIEFING","additional_context":"PROMPT CONTEXT"}' ;;
  garbage) printf '%s' 'this is not json' ;;
  crash)   exit 3 ;;
  *)       : ;;
esac
`

// nodeTestHookTimeoutMS is the bridge call budget these tests render.
//
// It is not the shipped 5000 ms, and the difference is the whole point.
// callHook is fail-open by contract: a bridge that does not answer inside
// the budget lets the tool through. So every case below that asserts a
// decision took EFFECT (block, tip, orient) is only really asserting that
// `/bin/sh` got scheduled in time — a property of the machine, not of the
// bridge. Under `go test -race ./...` it is routinely false: both
// TestPluginBehaviourUnderNode/block and
// TestPluginEnvelopeMatchesTheBridgeContract failed that way, and both
// reproduce exactly, message for message, by rendering a 1 ms budget
// instead. The bridge was correct every time.
//
// Widening it here weakens nothing. The three fail-open cases (crash,
// garbage, empty) do not depend on the budget at all, and the shipped
// default is still asserted through renderPlugin everywhere else.
const nodeTestHookTimeoutMS = 120_000

// driver exercises every hook the plugin exposes and reports what
// happened, so one node run covers the whole surface.
const driver = `import { GortexPlugin } from "./gortex.mjs";

const hooks = await GortexPlugin({ directory: process.cwd(), worktree: process.cwd() });
const result = { threw: null, toolOutput: null, promptText: null, permission: null };

try {
  await hooks["tool.execute.before"](
    { tool: "read", sessionID: "s1", callID: "c1" },
    { args: { filePath: "/repo/main.go" } },
  );
} catch (err) {
  result.threw = err.message;
}

const toolResult = { title: "read", output: "FILE CONTENTS", metadata: {} };
await hooks["tool.execute.after"]({ tool: "read", sessionID: "s1", callID: "c1", args: {} }, toolResult);
result.toolOutput = toolResult.output;

const parts = [{ type: "text", text: "why is login failing" }];
await hooks["chat.message"]({}, { message: { role: "user", sessionID: "s1" }, parts });
result.promptText = parts[0].text;

const permission = { status: "ask" };
await hooks["permission.ask"](
  { type: "bash", sessionID: "s1", callID: "c2", metadata: { command: "rm -rf /" } },
  permission,
);
result.permission = permission.status;

console.log(JSON.stringify(result));
`

type driverResult struct {
	Threw      *string `json:"threw"`
	ToolOutput string  `json:"toolOutput"`
	PromptText string  `json:"promptText"`
	Permission string  `json:"permission"`
}

func TestPluginBehaviourUnderNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook stub is a POSIX shell script")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}

	cases := []struct {
		mode  string
		check func(t *testing.T, got driverResult)
	}{
		{
			mode: "block",
			check: func(t *testing.T, got driverResult) {
				if got.Threw == nil || !strings.Contains(*got.Threw, "call search_symbols instead") {
					t.Fatalf("a block decision must throw the Go reason verbatim, got %v", got.Threw)
				}
				if got.Permission != "deny" {
					t.Fatalf("permission.ask should have denied, got %q", got.Permission)
				}
			},
		},
		{
			mode: "tip",
			check: func(t *testing.T, got driverResult) {
				if got.Threw != nil {
					t.Fatalf("soft guidance must not block: %v", *got.Threw)
				}
				if !strings.Contains(got.ToolOutput, "FILE CONTENTS") || !strings.Contains(got.ToolOutput, "GRAPH TIP") {
					t.Fatalf("the after-hook must append the parked tip to the result, got %q", got.ToolOutput)
				}
				if got.Permission != "ask" {
					t.Fatalf("a non-blocking decision must leave the permission status alone, got %q", got.Permission)
				}
			},
		},
		{
			mode: "orient",
			check: func(t *testing.T, got driverResult) {
				if !strings.HasPrefix(got.PromptText, "why is login failing") {
					t.Fatalf("injection must extend the user's text, not replace it: %q", got.PromptText)
				}
				if !strings.Contains(got.PromptText, "BRIEFING") || !strings.Contains(got.PromptText, "PROMPT CONTEXT") {
					t.Fatalf("chat.message must carry both the briefing and the turn context: %q", got.PromptText)
				}
			},
		},
		// The three fail-open modes: a broken bridge is indistinguishable
		// from no bridge at all.
		{mode: "crash", check: assertNoInterference},
		{mode: "garbage", check: assertNoInterference},
		{mode: "empty", check: assertNoInterference},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "capture.jsonl")

			stub := filepath.Join(dir, "gortex")
			if err := os.WriteFile(stub, []byte(stubHook), 0o755); err != nil {
				t.Fatal(err)
			}
			env, _ := agentstest.NewEnv(t)
			env.Mode = agents.ModeGlobal
			env.HookCommand = stub + " hook"
			if err := os.WriteFile(filepath.Join(dir, "gortex.mjs"),
				[]byte(renderPluginWithHookTimeout(env, nodeTestHookTimeoutMS)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(driver), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(node, "driver.mjs")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GORTEX_STUB_MODE="+tc.mode,
				"GORTEX_STUB_CAPTURE="+capture,
			)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("driver failed: %v\n%s", err, out)
			}
			var got driverResult
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("driver output %q: %v", out, err)
			}
			tc.check(t, got)
		})
	}
}

func assertNoInterference(t *testing.T, got driverResult) {
	t.Helper()
	if got.Threw != nil {
		t.Fatalf("a broken bridge must not block a tool call, but it threw %q", *got.Threw)
	}
	if got.ToolOutput != "FILE CONTENTS" {
		t.Fatalf("a broken bridge must not touch the tool result, got %q", got.ToolOutput)
	}
	if got.PromptText != "why is login failing" {
		t.Fatalf("a broken bridge must not touch the user's message, got %q", got.PromptText)
	}
	if got.Permission != "ask" {
		t.Fatalf("a broken bridge must not change a permission decision, got %q", got.Permission)
	}
}

// TestPluginEnvelopeMatchesTheBridgeContract reads back what the plugin
// actually put on the hook's stdin and checks it against the BridgeEvent
// fields in internal/hooks/pi.go — including the `filePath` -> `file_path`
// mapping, without which the Go classifier sees no path on any read and
// enforces nothing while looking perfectly healthy.
func TestPluginEnvelopeMatchesTheBridgeContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook stub is a POSIX shell script")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}

	dir := t.TempDir()
	capture := filepath.Join(dir, "capture.jsonl")
	stub := filepath.Join(dir, "gortex")
	if err := os.WriteFile(stub, []byte(stubHook), 0o755); err != nil {
		t.Fatal(err)
	}
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.HookCommand = stub + " hook"
	if err := os.WriteFile(filepath.Join(dir, "gortex.mjs"),
		[]byte(renderPluginWithHookTimeout(env, nodeTestHookTimeoutMS)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "driver.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GORTEX_STUB_MODE=empty", "GORTEX_STUB_CAPTURE="+capture)
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("driver failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("no envelope was ever written: %v", err)
	}
	envelopes := map[string]map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("envelope %q is not JSON: %v", line, err)
		}
		event, _ := ev["event"].(string)
		envelopes[event] = ev
	}

	for _, event := range []string{"tool.execute.before", "session", "chat.message", "permission.ask"} {
		if _, ok := envelopes[event]; !ok {
			t.Fatalf("no %q envelope was sent; got %v", event, keysOf(envelopes))
		}
	}

	before := envelopes["tool.execute.before"]
	if before["tool_name"] != "Read" {
		t.Fatalf("tool name must arrive in the Claude vocabulary, got %v", before["tool_name"])
	}
	input, _ := before["tool_input"].(map[string]any)
	if input["file_path"] != "/repo/main.go" {
		t.Fatalf("OpenCode's filePath was not mapped onto file_path: %v", input)
	}
	if before["session_id"] != "s1" {
		t.Fatalf("session_id missing: %v", before)
	}
	if before["cwd"] == "" || before["cwd"] == nil {
		t.Fatalf("cwd missing: %v", before)
	}

	perm := envelopes["permission.ask"]
	if perm["tool_name"] != "Bash" {
		t.Fatalf("permission type must map onto the Claude vocabulary, got %v", perm["tool_name"])
	}
	permInput, _ := perm["tool_input"].(map[string]any)
	if permInput["command"] != "rm -rf /" {
		t.Fatalf("permission metadata did not become tool_input: %v", permInput)
	}

	if prompt, _ := envelopes["chat.message"]["prompt"].(string); prompt != "why is login failing" {
		t.Fatalf("chat.message must carry the turn text, got %q", prompt)
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
