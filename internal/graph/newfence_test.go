package graph

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The in-memory Store is a staging buffer, not a persistence backend. The
// indexer parses a cold index into one and drains it into the durable store —
// that staging hop is worth several times the index wall-clock, which is why
// the type still exists outside tests. Everything else that needs a Store must
// take a real one.
//
// stagingCallers lists the non-test files allowed to construct it. Keeping the
// list this short is the point: a new entry means some production path is about
// to hold graph data that no restart can recover.
var stagingCallers = []string{
	"internal/indexer/indexer.go",
}

// fenceSkippedDirs are directory names the scan never descends into: version
// control, fixture trees that intentionally contain unbuildable Go, and vendored
// or generated third-party code.
var fenceSkippedDirs = map[string]struct{}{
	".git":         {},
	"testdata":     {},
	"vendor":       {},
	"node_modules": {},
}

func TestNewIsFencedToIndexerStaging(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locating repository root: %v", err)
	}

	allowed := make(map[string]struct{}, len(stagingCallers))
	for _, p := range stagingCallers {
		allowed[p] = struct{}{}
	}

	var offenders []string
	seen := make(map[string]struct{}, len(stagingCallers))

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := fenceSkippedDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		// This package calls New unqualified, so it never matches the scanned
		// pattern; skipping it keeps the scan honest about that.
		if path.Dir(rel) == "internal/graph" {
			return nil
		}
		src, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		if !callsGraphNew(string(src)) {
			return nil
		}
		if _, ok := allowed[rel]; ok {
			seen[rel] = struct{}{}
			return nil
		}
		offenders = append(offenders, rel)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scanning %s: %v", root, walkErr)
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("in-memory Store constructed outside the indexer's staging path:\n  %s\n\n"+
			"That Store keeps everything in process memory: nothing written to it survives a "+
			"restart, and no other process can read it. Production code that needs a graph should "+
			"accept a Store from its caller or open a durable one. If you really are adding an "+
			"indexer staging buffer that is drained into a durable store, add the file to "+
			"stagingCallers in this test and say why in the commit.",
			strings.Join(offenders, "\n  "))
	}

	for _, p := range stagingCallers {
		if _, ok := seen[p]; !ok {
			t.Errorf("%s no longer constructs the in-memory Store — drop it from stagingCallers", p)
		}
	}
}

// callsGraphNew reports whether src contains a qualified construction of the
// in-memory Store in executable code. Comments and string literals are stripped
// first so prose about the staging buffer does not trip the fence, and a match
// preceded by an identifier character (someothergraph.New()) is rejected.
func callsGraphNew(src string) bool {
	code := stripGoComments(src)
	const call = "graph.New()"
	for i := 0; ; {
		j := strings.Index(code[i:], call)
		if j < 0 {
			return false
		}
		at := i + j
		if at == 0 || !isFenceIdentByte(code[at-1]) {
			return true
		}
		i = at + len(call)
	}
}

func isFenceIdentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '.':
		return true
	}
	return false
}
