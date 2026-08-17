package hooks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/localizationauth"
)

// A refusal must never be the answer to advice this same hook just gave. When
// a localization marker is live, the access policy's "call a Gortex graph tool
// instead" would be met by the marker refusing exactly that call — two turns
// spent to learn nothing. These fixtures pin that the marker answers the host
// tool directly, and that it hands back the retained answer when it does.
func TestAdvisoryMarkerAnswersTheHostToolsItWouldOtherwiseRedirect(t *testing.T) {
	const answer = "LOCALIZATION (UNCONFIRMED):\n- PRIMARY — storage/disk.go:42 — repo/storage/disk.go::DiskStorage.Load"
	for _, tool := range []string{"Read", "Grep", "Glob"} {
		t.Run(tool, func(t *testing.T) {
			configureLocalizationTerminalTestHome(t)
			identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
			if !markLocalizationTerminalReceipt(identity, localizationauth.Receipt{FinalResponse: answer, ContractVersion: localizationTerminalContractV2}) {
				t.Fatal("advisory marker was not written")
			}
			input := map[string]any{"file_path": "storage/disk.go", "pattern": "Load"}
			pre := preToolPayload(t, tool, "advised-tool", identity, input)
			encoded := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeEnrich) })
			if encoded == "" {
				t.Fatalf("%s produced no decision under a live marker", tool)
			}
			var output HookOutput
			if err := json.Unmarshal([]byte(encoded), &output); err != nil {
				t.Fatalf("decode PreToolUse output %q: %v", encoded, err)
			}
			hso := output.HookSpecificOutput
			if hso == nil || hso.PermissionDecision != "deny" {
				t.Fatalf("%s was left to the access policy under a live marker: %#v", tool, hso)
			}
			if !strings.HasPrefix(hso.PermissionDecisionReason, localizationAdvisoryDenyReason) {
				t.Fatalf("%s deny reason = %q, want the advisory reason", tool, hso.PermissionDecisionReason)
			}
			// The whole point of answering here is that the caller gets the
			// answer, not another instruction.
			if !strings.Contains(hso.PermissionDecisionReason, answer) {
				t.Fatalf("%s deny did not carry the retained answer: %q", tool, hso.PermissionDecisionReason)
			}
			if strings.Contains(hso.PermissionDecisionReason, "instead") &&
				strings.Contains(hso.PermissionDecisionReason, "explore") {
				t.Fatalf("%s deny still prescribes a call the gate would refuse: %q", tool, hso.PermissionDecisionReason)
			}
		})
	}
}

// Tools the access policy would not redirect into a graph call keep passing
// through under an advisory marker — the change closes one contradiction, it
// does not widen the blast radius of a non-enforceable answer.
func TestAdvisoryMarkerStillPassesThroughUnrelatedTools(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
	if !markLocalizationTerminalReceipt(identity, localizationauth.Receipt{FinalResponse: "answer", ContractVersion: localizationTerminalContractV2}) {
		t.Fatal("advisory marker was not written")
	}
	for _, tool := range []string{"WebSearch", "Write"} {
		t.Run(tool, func(t *testing.T) {
			pre := preToolPayload(t, tool, "unrelated-tool", identity, map[string]any{"query": "storage"})
			encoded := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeEnrich) })
			if encoded == "" {
				return
			}
			var output HookOutput
			if err := json.Unmarshal([]byte(encoded), &output); err != nil {
				t.Fatalf("decode PreToolUse output %q: %v", encoded, err)
			}
			if output.HookSpecificOutput != nil &&
				output.HookSpecificOutput.PermissionDecision == "deny" &&
				strings.HasPrefix(output.HookSpecificOutput.PermissionDecisionReason, localizationAdvisoryDenyReason) {
				t.Fatalf("advisory marker denied an unrelated tool %s: %#v", tool, output.HookSpecificOutput)
			}
		})
	}
}
