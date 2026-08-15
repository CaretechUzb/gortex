package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docs_counts_test.go pins the adapter count printed in the docs to the
// registry itself.
//
// Three separate files advertise how many agent integrations ship, and
// all three had drifted — the docs still said nineteen while the
// registry had grown. A count nobody can verify is worse than no count:
// a reader who spots one stale number stops trusting the rest of the
// page. Deriving it here means the next adapter fails this test until
// its author updates every place that claims a total.

// countedDocs maps a repo-relative doc to the phrasing it uses for the
// adapter total. Each entry must contain the numeral, so a single
// substring check covers all three without parsing prose.
var countedDocs = []struct {
	path    string
	context string // human hint printed on failure
}{
	{"docs/agents.md", "\"NN adapters ship today\" in the intro"},
	{"docs/skills.md", "\"NN other AI coding assistants\" under Usage with other agents"},
	{"README.md", "\"Agent integrations (NN)\" and the docs table row"},
}

func TestDocsAdapterCountMatchesRegistry(t *testing.T) {
	want := len(buildRegistry().All())
	if want == 0 {
		t.Fatal("buildRegistry() returned no adapters — the guard would pass vacuously")
	}

	root := repoRootForDocs(t)
	for _, doc := range countedDocs {
		t.Run(doc.path, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, doc.path))
			if err != nil {
				t.Fatalf("read %s: %v", doc.path, err)
			}
			// docs/skills.md counts the OTHER assistants — every adapter
			// except claude-code, which the page describes separately.
			n := want
			if doc.path == "docs/skills.md" {
				n = want - 1
			}
			if !strings.Contains(string(body), fmt.Sprint(n)) {
				t.Errorf("%s does not mention the adapter count %d; update %s", doc.path, n, doc.context)
			}
		})
	}
}

// repoRootForDocs walks up from the test's working directory (cmd/gortex)
// to the module root, so the guard works from any invocation directory.
func repoRootForDocs(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test working directory")
		}
		dir = parent
	}
}
