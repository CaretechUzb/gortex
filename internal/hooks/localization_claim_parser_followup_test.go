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

func TestLocalizationClaimParserRespectsIndentedCode(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{name: "zero-space Setext", message: "Fabricated.flush\n---"},
		{name: "three-space Setext", message: "   Fabricated.flush\n   ---"},
		{name: "four-space code", message: "    Fabricated.flush\n    ---", want: []string{"Fabricated.flush"}},
		{name: "tab-indented code", message: "\tFabricated.flush\n\t---", want: []string{"Fabricated.flush"}},
		{name: "top-level claim and code underline", message: "Fabricated.flush\n    ---", want: []string{"Fabricated.flush"}},
		{name: "quoted Setext", message: "> Fabricated.flush\n> ---"},
		{name: "different quote containers", message: "> Fabricated.flush\n>> ---", want: []string{"Fabricated.flush"}},
		{name: "structured claim before thematic break", message: "SYMBOLS:\n- Fabricated.flush\n---", want: []string{"Fabricated.flush"}},
		{name: "list claim before thematic break", message: "- Fabricated.flush\n---", want: []string{"Fabricated.flush"}},
		{name: "atx heading", message: "# Fabricated.flush"},
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

func TestLocalizationClaimParserKeepsAmbiguousExtensionSymbols(t *testing.T) {
	for _, identity := range []string{"obj.m", "module.r", "node.s", "pkg.v", "value.d"} {
		t.Run(identity+" symbol", func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims("The owner is " + identity + ".")
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, []string{identity})
		})
		t.Run(identity+" file context", func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims("The source file is " + identity + ".")
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, nil)
		})
		t.Run(identity+" structured", func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims("SYMBOLS:\n- " + identity)
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, []string{identity})
		})
	}
	claims, _, valid := localizationBoundedSymbolClaims("See file foo.c.")
	if !valid {
		t.Fatal("C file message was rejected")
	}
	assertLocalizationClaims(t, claims, nil)
}

func TestLocalizationStructuredClaimsNormalizeSentencePunctuation(t *testing.T) {
	for _, test := range []struct {
		row  string
		want string
	}{
		{row: "flush,", want: "flush"},
		{row: "Writer;", want: "Writer"},
		{row: "_private.", want: "_private"},
		{row: "$foo:", want: "$foo"},
		{row: "`flush`,", want: "flush"},
		{row: "flush(),", want: "flush"},
	} {
		t.Run(test.row, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims("SYMBOLS:\n- " + test.row)
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, []string{test.want})
		})
	}
	claims, _, valid := localizationBoundedSymbolClaims("SYMBOLS:\n- Writer performs the work.")
	if !valid {
		t.Fatal("prose row was rejected")
	}
	assertLocalizationClaims(t, claims, nil)
}

func TestLocalizationClaimParserRecognizesStableFileVocabulary(t *testing.T) {
	files := []string{
		"README", "LICENSE", "CHANGELOG", "Dockerfile.ci", "Makefile.am", "CMakeLists.txt",
		"go.mod", "go.sum", "go.work", "Cargo.lock", "notebook.ipynb", "contract.sol",
		"module.move", "program.cairo", "circuit.noir", "contract.tact", "service.bal",
		"settings.toml", "page.vue", "template.gotmpl", "document.adoc",
	}
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			claims, _, valid := localizationBoundedSymbolClaims("See " + file + ".")
			if !valid {
				t.Fatal("message was rejected")
			}
			assertLocalizationClaims(t, claims, nil)
		})
	}
	for _, test := range []struct {
		message string
		want    string
	}{
		{message: "The owner is writer.flush.", want: "writer.flush"},
		{message: "The owner is obj.m.", want: "obj.m"},
		{message: "The owner is file.ext::symbol.", want: "file.ext::symbol"},
	} {
		claims, _, valid := localizationBoundedSymbolClaims(test.message)
		if !valid {
			t.Fatalf("message %q was rejected", test.message)
		}
		assertLocalizationClaims(t, claims, []string{test.want})
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
