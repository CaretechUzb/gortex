package hooks

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLocalizationClaimCheckPersistentSidecarDoesNotWedge(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- Fabricated.flush")
	identity, ok := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ok {
		t.Fatal("localization turn missing")
	}
	lockPath := localizationTerminalMarkerPath(identity) + ".claim-check"
	if err := os.WriteFile(lockPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if got := localizationClaimCheck(input); got == "" {
		t.Fatal("persistent stale lock sidecar wedged claim checking")
	}
}

func TestLocalizationClaimCheckConcurrentPreToolUseStaysDenied(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- Fabricated.flush")
	identity, ok := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ok {
		t.Fatal("localization turn missing")
	}
	release, ok := acquireLocalizationClaimCheck(localizationTerminalMarkerPath(identity))
	if !ok {
		t.Fatal("claim-check lock was not acquired")
	}
	defer release()

	payload := preToolPayload(t, "Read", "concurrent-read", identity, map[string]any{"file_path": "repo/a.go"})
	output := captureHookStdout(t, func() { runPreToolUse(payload, 0, ModeDeny) })
	if !strings.Contains(output, localizationTerminalDenyReason) || !strings.Contains(output, `"permissionDecision":"deny"`) {
		t.Fatalf("concurrent PreToolUse bypassed terminal marker: %q", output)
	}
}
