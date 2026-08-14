package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCodexStopClaimCheckUsesFinalMessageAndStopsAfterRetry(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- flush")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runCodex(data, 0) })
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode claim-check output %q: %v", out, err)
	}
	if payload.Decision != "block" || !strings.Contains(payload.Reason, "claim_check") {
		t.Fatalf("Codex Stop was not blocked with a claim check: %#v", payload)
	}

	input.StopHookActive = true
	data, _ = json.Marshal(input)
	if retry := captureStdout(t, func() { runCodex(data, 0) }); retry != "" {
		t.Fatalf("recursive Codex Stop should be silent: %q", retry)
	}
}

func TestRunCodexStopClaimCheckReadsPrimaryHeading(t *testing.T) {
	input := claimCheckTestInput(t, "# PRIMARY\nFabricated")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runCodex(data, 0) })
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode heading claim-check output %q: %v", out, err)
	}
	if payload.Decision != "block" || !strings.Contains(payload.Reason, "claim_check") {
		t.Fatalf("Codex Stop did not enforce PRIMARY heading claims: %#v", payload)
	}
}

func TestRunCodexStopFailsOpenWithoutFinalMessage(t *testing.T) {
	input := claimCheckTestInput(t, "")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() { runCodex(data, 0) }); out != "" {
		t.Fatalf("Codex Stop without last_assistant_message must fail open: %q", out)
	}
}
