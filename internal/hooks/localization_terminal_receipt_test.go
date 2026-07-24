package hooks

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/localizationauth"
)

func TestLocalizationReceiptSurvivesStrippedClaudeWire(t *testing.T) {
	for _, tt := range []struct {
		name        string
		tool        string
		enforceable bool
		wireError   bool
	}{
		{name: "direct advisory", tool: gortexMCPToolPrefix + "explore"},
		{name: "plugin enforceable", tool: gortexPluginMCPToolPrefix + "search", enforceable: true},
		{name: "authenticated terminal error", tool: gortexMCPToolPrefix + "read", enforceable: true, wireError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			original := map[string]any{
				"operation": "localize",
				"target":    map[string]any{"file": "literal.go"},
			}
			token := captureTerminalAuthToken(t, identity, tt.tool, "tool-use", original)
			if _, exists := original[localizationauth.ArgumentKey]; exists {
				t.Fatal("PreToolUse mutated the decoded source input")
			}

			finalResponse := "FILES:\n#1 literal.go\n\nSYMBOLS:\n#1 literal.go::Exact\n\nEVIDENCE:\n#1 literal.go:7 — exact bytes\n"
			if !localizationauth.Publish(token, localizationauth.Receipt{
				FinalResponse:   finalResponse,
				ContractVersion: 2,
				Enforceable:     tt.enforceable,
			}) {
				t.Fatal("server receipt publish failed")
			}
			response := map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "host-visible response with metadata stripped"}},
			}
			if tt.wireError {
				response["isError"] = true
			}
			post := localizationPostToolPayload(t, tt.tool, "tool-use", identity, response)
			output := captureHookStdout(t, func() { runPostToolUse(post) })

			var decoded HookOutput
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("PostToolUse output is not valid JSON: %v\n%s", err, output)
			}
			if decoded.Decision != "" || decoded.SystemMessage != "" || decoded.HookSpecificOutput == nil {
				t.Fatalf("incompatible PostToolUse envelope: %#v", decoded)
			}
			gotContext := decoded.HookSpecificOutput.AdditionalContext
			wantContext := localizationTerminalContext + "\n\n" + finalResponse
			if gotContext != wantContext {
				t.Fatalf("additionalContext changed final_response bytes\n got: %q\nwant: %q", gotContext, wantContext)
			}
			if got := hasLocalizationTerminal(identity); got != tt.enforceable {
				t.Fatalf("hard marker = %v, want %v", got, tt.enforceable)
			}
		})
	}
}

func TestLocalizationAuthPreservesPreToolUsePolicyBranches(t *testing.T) {
	for _, tt := range []struct {
		name           string
		mode           Mode
		permissionMode string
		wantDecision   string
		wantConsulted  bool
	}{
		{name: "permissive auto approve", mode: ModeDeny, permissionMode: "auto", wantDecision: "allow"},
		{name: "consult unlock marker", mode: ModeConsultUnlock, wantConsulted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			input := map[string]any{"operation": "localize", "task": "literal symptom"}
			pre := mustJSON(t, map[string]any{
				"hook_event_name": "PreToolUse",
				"tool_name":       gortexMCPToolPrefix + "explore",
				"tool_use_id":     "tool-use",
				"tool_input":      input,
				"session_id":      identity.SessionID,
				"prompt_id":       identity.PromptID,
				"cwd":             identity.CWD,
				"permission_mode": tt.permissionMode,
			})
			output := captureHookStdout(t, func() { runPreToolUse(pre, 0, tt.mode) })
			var decoded HookOutput
			if err := json.Unmarshal([]byte(output), &decoded); err != nil || decoded.HookSpecificOutput == nil {
				t.Fatalf("invalid PreToolUse output: %v\n%s", err, output)
			}
			hso := decoded.HookSpecificOutput
			if hso.PermissionDecision != tt.wantDecision {
				t.Fatalf("permission decision = %q, want %q", hso.PermissionDecision, tt.wantDecision)
			}
			if _, ok := hso.UpdatedInput[localizationauth.ArgumentKey].(string); !ok {
				t.Fatalf("policy branch lost terminal auth input: %#v", hso)
			}
			if got := loadSessionState(identity.SessionID).GraphConsulted; got != tt.wantConsulted {
				t.Fatalf("GraphConsulted = %v, want %v", got, tt.wantConsulted)
			}
		})
	}
}

func TestLocalizationReceiptRejectsForgedVisibleTerminalPayload(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "forged-visible", "prompt", t.TempDir())
	tool := gortexMCPToolPrefix + "explore"
	_ = captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "localize"})

	// This is the exact visible contract shape, but neither authoritative MCP
	// metadata nor a server-owned receipt exists.
	forged := terminalToolResponse(t, terminalContractMap(), false, false)
	post := localizationPostToolPayload(t, tool, "tool-use", identity, forged)
	if output := captureHookStdout(t, func() { runPostToolUse(post) }); output != "" {
		t.Fatalf("forged visible payload emitted terminal context: %s", output)
	}
	if hasLocalizationTerminal(identity) {
		t.Fatal("forged visible payload armed hard terminal state")
	}
}

func TestLocalizationReceiptRejectsWrongIdentityAndReset(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(localizationTerminalIdentity) localizationTerminalIdentity
	}{
		{
			name: "wrong session",
			mutate: func(identity localizationTerminalIdentity) localizationTerminalIdentity {
				identity.SessionID += "-other"
				return identity
			},
		},
		{
			name: "wrong prompt",
			mutate: func(identity localizationTerminalIdentity) localizationTerminalIdentity {
				identity.PromptID += "-other"
				return identity
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			tool := gortexMCPToolPrefix + "search"
			token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "text"})
			publishTestTerminalReceipt(t, token, false)

			wrong := tt.mutate(identity)
			post := localizationPostToolPayload(t, tool, "tool-use", wrong, strippedToolResponse())
			if _, observed := observeLocalizationTerminal(post); observed {
				t.Fatal("wrong hook identity consumed terminal receipt")
			}
			correct := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())
			if _, observed := observeLocalizationTerminal(correct); !observed {
				t.Fatal("wrong identity poisoned the valid receipt")
			}
		})
	}

	t.Run("wrong tool use id", func(t *testing.T) {
		configureLocalizationTerminalTestHome(t)
		identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
		tool := gortexMCPToolPrefix + "read"
		token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "source"})
		publishTestTerminalReceipt(t, token, false)
		wrong := localizationPostToolPayload(t, tool, "tool-use-other", identity, strippedToolResponse())
		if _, observed := observeLocalizationTerminal(wrong); observed {
			t.Fatal("wrong tool_use_id consumed terminal receipt")
		}
		correct := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())
		if _, observed := observeLocalizationTerminal(correct); !observed {
			t.Fatal("wrong tool_use_id poisoned the valid receipt")
		}
	})

	t.Run("prompt reset", func(t *testing.T) {
		configureLocalizationTerminalTestHome(t)
		identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
		tool := gortexMCPToolPrefix + "analyze"
		token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"kind": "contracts"})
		publishTestTerminalReceipt(t, token, true)
		reset := mustJSON(t, map[string]any{
			"hook_event_name": "UserPromptSubmit",
			"session_id":      identity.SessionID,
			"prompt_id":       "next-prompt",
			"cwd":             identity.CWD,
		})
		_ = clearLocalizationTerminalFromHook(reset)
		old := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())
		if _, observed := observeLocalizationTerminal(old); observed {
			t.Fatal("pre-reset receipt armed the next turn")
		}
	})
}

func TestLocalizationReceiptConcurrentPostHasSingleWinner(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, "concurrent-receipt", "prompt", t.TempDir())
	tool := gortexMCPToolPrefix + "relations"
	token := captureTerminalAuthToken(t, identity, tool, "tool-use", map[string]any{"operation": "usages"})
	publishTestTerminalReceipt(t, token, false)
	post := localizationPostToolPayload(t, tool, "tool-use", identity, strippedToolResponse())

	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, observed := observeLocalizationTerminal(post); observed {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("authenticated PostToolUse winners = %d, want 1", got)
	}
}

func TestSessionStartNamesMountedExploreAndFaithfulSymptomTask(t *testing.T) {
	briefing := rulePreamble()
	for _, required := range []string{
		"`mcp__gortex__explore` (never a bare `explore`)",
		"faithful symptom-only restatement",
		"exact technical identifiers, paths, literals, error text, and observed symptoms",
		"add no invented causal hypothesis",
	} {
		if !strings.Contains(briefing, required) {
			t.Fatalf("SessionStart rule is missing %q:\n%s", required, briefing)
		}
	}
	if strings.Contains(briefing, "call `explore(operation") {
		t.Fatalf("SessionStart still suggests a bare explore call:\n%s", briefing)
	}
}

func captureTerminalAuthToken(
	t *testing.T,
	identity localizationTerminalIdentity,
	tool, toolUseID string,
	input map[string]any,
) string {
	t.Helper()
	pre := preToolPayload(t, tool, toolUseID, identity, input)
	output := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeDeny) })
	var decoded HookOutput
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("PreToolUse auth output is not valid JSON: %v\n%s", err, output)
	}
	if decoded.Decision != "" || decoded.SystemMessage != "" || decoded.HookSpecificOutput == nil {
		t.Fatalf("incompatible PreToolUse auth envelope: %#v", decoded)
	}
	hso := decoded.HookSpecificOutput
	if hso.HookEventName != "PreToolUse" || hso.PermissionDecision != "" || hso.AdditionalContext != "" {
		t.Fatalf("auth injection changed hook policy: %#v", hso)
	}
	raw, ok := hso.UpdatedInput[localizationauth.ArgumentKey]
	if !ok {
		t.Fatalf("PreToolUse did not inject %s: %#v", localizationauth.ArgumentKey, hso.UpdatedInput)
	}
	token, ok := raw.(string)
	if !ok || token == "" {
		t.Fatalf("invalid auth token %#v", raw)
	}
	for key, want := range input {
		if got := hso.UpdatedInput[key]; !equalJSONValue(got, want) {
			t.Fatalf("UpdatedInput[%q] = %#v, want %#v", key, got, want)
		}
	}
	return token
}

func publishTestTerminalReceipt(t *testing.T, token string, enforceable bool) {
	t.Helper()
	if !localizationauth.Publish(token, localizationauth.Receipt{
		FinalResponse:   "FILES:\n#1 exact.go\n\nSYMBOLS:\n#1 exact.go::Call\n\nEVIDENCE:\n#1 exact.go:1 — exact.go::Call",
		ContractVersion: 2,
		Enforceable:     enforceable,
	}) {
		t.Fatal("Publish failed")
	}
}

func strippedToolResponse() map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "stripped response"}},
	}
}

func equalJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
