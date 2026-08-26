package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// embeddedTextAssets are the text files this tree hands to a user
// verbatim: go:embed compiles them into the binary and the adapters write
// them into a project unchanged. Their bytes are therefore a shipping
// artefact, not a source detail, and a checkout that rewrites their line
// endings changes what users receive.
var embeddedTextAssets = []string{
	"internal/agents/opencode/plugin/gortex.js",
	"internal/agents/pi/extension/index.ts",
}

// TestEmbeddedTextAssetsArePinnedToLF guards the `* text=auto eol=lf` line
// in .gitattributes from both directions.
//
// The byte half catches a checkout that already converted: Git for Windows
// installs with core.autocrlf=true, so without the attribute these files
// arrive CRLF and the binary ships CRLF. The attribute half catches the
// removal itself, and is the half that still binds on a runner configured
// not to convert — where a byte assertion would pass no matter what
// .gitattributes says.
//
// TestPluginFailsOpen detects the same corruption, but only as a side
// effect of splitting the plugin source on "\n}\n"; this states the
// contract directly and covers the pi extension too.
func TestEmbeddedTextAssetsArePinnedToLF(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Skipf("not a git work tree, nothing to assert: %v", err)
	}

	for _, rel := range embeddedTextAssets {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if i := strings.Index(string(body), "\r\n"); i >= 0 {
			t.Errorf("%s: embedded asset has a CRLF line ending at byte %d — "+
				"this checkout converted it and the binary would ship it that way; "+
				"check the `* text=auto eol=lf` line in .gitattributes", rel, i)
		}

		out, err := exec.Command("git", "-C", root, "check-attr", "eol", "--", rel).Output()
		if err != nil {
			t.Errorf("%s: git check-attr: %v", rel, err)
			continue
		}
		// `<path>: eol: <value>`; unset attributes report "unspecified".
		line := string(out)
		if got := strings.TrimSpace(line[strings.LastIndex(line, ":")+1:]); got != "lf" {
			t.Errorf("%s: eol attribute is %q, want \"lf\" — a Windows checkout "+
				"will convert this embedded asset to CRLF", rel, got)
		}
	}
}

// repoRoot locates the work tree so the test can address assets by their
// repo-relative path from whatever directory `go test` runs it in.
func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
