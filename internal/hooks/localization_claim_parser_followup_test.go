package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gofrs/flock"
)

func TestLocalizationClaimParserPairsSetextWithinOneContainer(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{name: "top level heading", message: "Fabricated.flush\n---"},
		{name: "quoted heading", message: "> Fabricated.flush\n> ==="},
		{name: "structured row before thematic break", message: "SYMBOLS:\n- Fabricated.flush\n---", want: []string{"Fabricated.flush"}},
		{name: "list row before thematic break", message: "- Fabricated.flush\n---", want: []string{"Fabricated.flush"}},
		{name: "different quote container", message: "Fabricated.flush\n> ---", want: []string{"Fabricated.flush"}},
		{name: "atx heading", message: "## Fabricated.flush"},
		{name: "fenced heading-like code", message: "```text\n# Fabricated.flush\n```", want: []string{"Fabricated.flush"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, test.want)
		})
	}
}

func TestLocalizationClaimParserPreservesIdentifierEdgeCharacters(t *testing.T) {
	identities := []string{"_private", "$foo", "_Writer.write", "Writer.write_", "$Writer#write"}
	for _, identity := range identities {
		t.Run(identity, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims("SYMBOLS:\n- `" + identity + "`()")
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, []string{identity})
			if !localizationClaimMatchesEvidence(identity, "repo/a.go::"+identity) {
				t.Fatalf("exact identity %q did not authenticate", identity)
			}
			trimmed := strings.Trim(identity, "_$")
			if trimmed != identity && localizationClaimMatchesEvidence(identity, "repo/a.go::"+trimmed) {
				t.Fatalf("identity %q authenticated after edge characters were removed", identity)
			}
		})
	}
}

func TestLocalizationClaimParserRequiresExplicitSyntaxForSimpleProseNames(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{name: "plain prose leaf", message: "The answer relies on flush."},
		{name: "inline code leaf", message: "The answer relies on `flush`.", want: []string{"flush"}},
		{name: "inline code call", message: "The answer relies on `flush()`.", want: []string{"flush"}},
		{name: "call expression", message: "The answer relies on flush().", want: []string{"flush"}},
		{name: "unmatched inline delimiter", message: "The answer relies on `flush."},
		{name: "file-qualified leaf", message: "The answer relies on repo/a.go::flush.", want: []string{"repo/a.go::flush"}},
		{name: "benign parenthetical prose", message: "The answer is (ordinary)."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, test.want)
		})
	}
}

func TestLocalizationClaimParserDistinguishesFilesFromSymbols(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{name: "c source", message: "See foo.c."},
		{name: "markdown document", message: "See README.md."},
		{name: "sql schema", message: "See schema.sql."},
		{name: "vue component", message: "See Component.vue."},
		{name: "slash path", message: "See internal/parser/file.custom."},
		{name: "qualified symbol", message: "The owner is writer.flush.", want: []string{"writer.flush"}},
		{name: "file-qualified symbol", message: "The owner is foo.c::flush.", want: []string{"foo.c::flush"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims(test.message)
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, test.want)
		})
	}
}

func TestLocalizationClaimLockFailureTelemetryDoesNotClaimContext(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "effectiveness.jsonl")
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", logPath)
	lock := flock.New(filepath.Join(t.TempDir(), "claim-check.lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("initial lock failed: locked=%v err=%v", locked, err)
	}
	if releaseLocalizationClaimLockObserved(lock, func(*flock.Flock) error { return syscall.EINTR }) {
		t.Fatal("persistent EINTR was reported as a successful release")
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("test cleanup release failed: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read release telemetry: %v", err)
	}
	record := string(data)
	if !strings.Contains(record, `"event":"LocalizationTerminal.claim_lock_release_failed"`) ||
		!strings.Contains(record, `"emitted_context":false`) {
		t.Fatalf("release failure telemetry has wrong context semantics: %s", record)
	}
}

func assertLocalizationClaims(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("claims = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("claims = %v, want %v", got, want)
		}
	}
}
