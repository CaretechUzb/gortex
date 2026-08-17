package hooks

import (
	"testing"

	"github.com/zzet/gortex/internal/localizationauth"
)

func TestLocalizationClaimCheckSkipsAdvisoryAuthority(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
	ids := []string{"repo/a.go::Writer.write"}
	if !markLocalizationTerminalReceipt(identity, localizationauth.Receipt{
		FinalResponse: "Possible answer", PrimaryIDs: ids, EvidenceIDs: ids,
		ContractVersion: localizationTerminalContractV2, Enforceable: false,
	}) {
		t.Fatal("advisory terminal marker was not written")
	}
	input := PostTaskInput{
		HookEventName: "Stop", SessionID: identity.SessionID, PromptID: identity.PromptID,
		AgentID: identity.AgentID, CWD: identity.CWD, LastAssistantMessage: "SYMBOLS:\n- flush",
	}
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("advisory evidence must fail open, got %q", got)
	}
}

func TestLocalizationClaimCheckPersistsOneShotConsumption(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- flush")
	if first := localizationClaimCheck(input); first == "" {
		t.Fatal("first unsupported claim was not challenged")
	}
	identity, ok := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ok {
		t.Fatal("localization turn disappeared")
	}
	marker, ok := localizationTerminalMarkerFor(identity)
	if !ok || !marker.ClaimCheckConsumed {
		t.Fatalf("claim-check consumption was not persisted: %#v ok=%v", marker, ok)
	}
	// Hosts are not uniformly reliable about stop_hook_active. Persisted state,
	// rather than that callback hint alone, makes a false retry acceptable.
	input.StopHookActive = false
	if retry := localizationClaimCheck(input); retry != "" {
		t.Fatalf("persisted claim check fired twice: %q", retry)
	}
}
