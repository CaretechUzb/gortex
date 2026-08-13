package hooks

import (
	"errors"
	"testing"

	"github.com/gofrs/flock"
)

func TestLocalizationClaimCheckUnionsStructuredAndProseClaims(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{
			name:    "authenticated structured plus fabricated prose",
			message: "SYMBOLS:\n- Writer.write\n\nThe fix is Fabricated.flush.",
		},
		{
			name:    "none fits plus fabricated prose",
			message: "SYMBOLS:\n- none fits\n\nThe fix is Fabricated.flush.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := claimCheckTestInput(t, test.message)
			if got := localizationClaimCheck(input); got == "" {
				t.Fatal("fabricated prose outside SYMBOLS was not challenged")
			}
		})
	}
}

func TestLocalizationClaimIdentityPreservesCaseAndSeparators(t *testing.T) {
	tests := []struct {
		name       string
		claim      string
		evidenceID string
	}{
		{name: "file path case", claim: "repo/A.go::Writer.write", evidenceID: "repo/a.go::Writer.write"},
		{name: "hash does not match dot", claim: "Writer#write", evidenceID: "repo/x.go::Writer.write"},
		{name: "dot does not match hash", claim: "Writer.write", evidenceID: "repo/x.py::Writer#write"},
		{name: "double colon does not match dot", claim: "Writer::write", evidenceID: "repo/x.go::Writer.write"},
		{name: "dot does not match double colon", claim: "Writer.write", evidenceID: "repo/x.rs::Writer::write"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if localizationClaimMatchesEvidence(test.claim, test.evidenceID) {
				t.Fatalf("claim %q cross-authenticated as %q", test.claim, test.evidenceID)
			}
		})
	}
	for _, claim := range []string{"write", "Writer.write", "(*Writer).write"} {
		if !localizationClaimMatchesEvidence(claim, "repo/a.go::Writer.write") {
			t.Errorf("documented claim notation %q did not authenticate", claim)
		}
	}
}

func TestLocalizationClaimParserKeepsColonEndedMaterialClaims(t *testing.T) {
	for _, message := range []string{
		"- Fabricated.flush:",
		"The culprit is Fabricated.flush:",
	} {
		claims, _, valid := localizationBoundedSymbolClaims(message)
		if !valid {
			t.Fatalf("message %q was rejected", message)
		}
		found := false
		for _, claim := range claims {
			if claim == "Fabricated.flush" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("colon-ended material claim was ignored: message=%q claims=%v", message, claims)
		}
	}
}

func TestLocalizationClaimLockReleaseFailureFallsBackToConcreteClose(t *testing.T) {
	path := t.TempDir() + "/claim-check.lock"
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("initial lock failed: locked=%v err=%v", locked, err)
	}
	releaseLocalizationClaimLockWith(lock, func(*flock.Flock) error {
		return errors.New("injected release failure")
	})

	challenger := flock.New(path)
	locked, err = challenger.TryLock()
	if err != nil || !locked {
		t.Fatalf("fallback Close retained ownership: locked=%v err=%v", locked, err)
	}
	if err := challenger.Close(); err != nil {
		t.Fatalf("challenger release failed: %v", err)
	}
}
