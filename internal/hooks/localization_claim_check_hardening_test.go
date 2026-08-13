package hooks

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestLocalizationClaimCheckPreservesQualifiedIdentity(t *testing.T) {
	tests := []struct {
		name       string
		claim      string
		evidenceID string
	}{
		{name: "owner", claim: "Other.write", evidenceID: "repo/a.go::Writer.write"},
		{name: "file", claim: "repo/z.go::Writer.write", evidenceID: "repo/a.go::Writer.write"},
		{name: "rust owner", claim: "module::Other::write", evidenceID: "repo/x.rs::module::Writer::write"},
		{name: "php namespace", claim: `Other\Writer::write`, evidenceID: `repo/y.php::App\Writer::write`},
		{name: "hash owner", claim: "Other#write", evidenceID: "repo/z.py::Writer#write"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := claimCheckTestInputWithEvidence(t, "SYMBOLS:\n- "+test.claim, []string{test.evidenceID}, []string{test.evidenceID})
			if got := localizationClaimCheck(input); got == "" {
				t.Fatalf("qualified homonym %q authenticated as %q", test.claim, test.evidenceID)
			}
		})
	}
}

func TestLocalizationClaimCheckAcceptsQualifiedLanguageNotation(t *testing.T) {
	tests := []struct {
		claim      string
		evidenceID string
	}{
		{claim: "module::Writer::write", evidenceID: "repo/x.rs::module::Writer::write"},
		{claim: `App\Writer::write`, evidenceID: `repo/y.php::App\Writer::write`},
		{claim: "Writer#write", evidenceID: "repo/z.py::Writer#write"},
	}
	for _, test := range tests {
		t.Run(test.claim, func(t *testing.T) {
			input := claimCheckTestInputWithEvidence(t, "SYMBOLS:\n- "+test.claim, []string{test.evidenceID}, []string{test.evidenceID})
			if got := localizationClaimCheck(input); got != "" {
				t.Fatalf("authenticated language-qualified claim was challenged: %q", got)
			}
		})
	}
}

func TestLocalizationClaimParserBounds(t *testing.T) {
	prefix := "SYMBOLS:\n- Writer.write"
	exactMessage := prefix + strings.Repeat(" ", localizationClaimCheckMaxMessageBytes-len(prefix))
	claims, _, valid := localizationBoundedSymbolClaims(exactMessage)
	if !valid || len(claims) != 1 {
		t.Fatalf("message at byte boundary rejected: valid=%v claims=%v", valid, claims)
	}
	if _, _, valid := localizationBoundedSymbolClaims(exactMessage + " "); valid {
		t.Fatal("message above byte boundary was accepted")
	}

	var claimLines strings.Builder
	claimLines.WriteString("SYMBOLS:\n")
	for index := 0; index < localizationClaimCheckMaxClaims; index++ {
		fmt.Fprintf(&claimLines, "- Owner%d.write\n", index)
	}
	claims, _, valid = localizationBoundedSymbolClaims(claimLines.String())
	if !valid || len(claims) != localizationClaimCheckMaxClaims {
		t.Fatalf("claim-count boundary rejected: valid=%v claims=%d", valid, len(claims))
	}
	claimLines.WriteString("- Overflow.write\n")
	if _, _, valid := localizationBoundedSymbolClaims(claimLines.String()); valid {
		t.Fatal("claim count above boundary was accepted")
	}

	exactToken := strings.Repeat("A", localizationClaimCheckMaxTokenBytes)
	if _, _, valid := localizationBoundedSymbolClaims("SYMBOLS:\n- " + exactToken); !valid {
		t.Fatal("claim token at byte boundary was rejected")
	}
	if _, _, valid := localizationBoundedSymbolClaims("SYMBOLS:\n- " + exactToken + "A"); valid {
		t.Fatal("claim token above byte boundary was accepted")
	}

	exactTokens := strings.Repeat("word ", localizationClaimCheckMaxTokens)
	if _, _, valid := localizationBoundedSymbolClaims(exactTokens); !valid {
		t.Fatal("unstructured token count at boundary was rejected")
	}
	if _, _, valid := localizationBoundedSymbolClaims(exactTokens + "word"); valid {
		t.Fatal("unstructured token count above boundary was accepted")
	}
}

func TestLocalizationClaimCheckFailsClosedOnOversizedMessage(t *testing.T) {
	input := claimCheckTestInput(t, strings.Repeat(" ", localizationClaimCheckMaxMessageBytes+1))
	if got := localizationClaimCheck(input); got == "" {
		t.Fatal("oversized untrusted final message was not challenged")
	}
}

func TestLocalizationClaimCheckLockKeepsTerminalMarkerVisible(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- Fabricated.flush")
	identity, ok := currentLocalizationTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ok {
		t.Fatal("localization turn missing")
	}
	release, ok := acquireLocalizationClaimCheck(localizationTerminalMarkerPath(identity))
	if !ok {
		t.Fatal("claim-check lock was not acquired")
	}

	const readers = 64
	missing := make(chan struct{}, readers)
	var wait sync.WaitGroup
	for index := 0; index < readers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if !hasLocalizationTerminal(identity) {
				missing <- struct{}{}
			}
		}()
	}
	wait.Wait()
	if len(missing) != 0 {
		release()
		t.Fatalf("%d concurrent readers observed a missing terminal marker", len(missing))
	}
	if got := localizationClaimCheck(input); got != "" {
		release()
		t.Fatalf("contending claim check did not fail open: %q", got)
	}
	release()
	if !hasLocalizationTerminal(identity) {
		t.Fatal("terminal marker disappeared after releasing claim-check lock")
	}
	if got := localizationClaimCheck(input); got == "" {
		t.Fatal("claim check was not available after releasing lock")
	}
}
