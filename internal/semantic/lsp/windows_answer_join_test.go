package lsp

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Graph FilePaths carry both separator spellings across store vintages —
// older Windows rows use `\` after the repo prefix while newer rows and
// every URI-derived path use `/` — so each join between a server answer
// and a graph node must match across spellings. Before these pins, every
// LSP answer on a backslash-spelled store missed its node: zero yield
// from a perfectly answering server, and the productivity checkpoint then
// cancelled the pass.

func TestGraphViewLookupsMatchAcrossSeparatorSpellings(t *testing.T) {
	iface := &graph.Node{ID: "n1", Kind: graph.KindInterface, Name: "IThing",
		FilePath: `repo/pkg\IThing.cs`, StartLine: 5, EndLine: 20}
	method := &graph.Node{ID: "n2", Kind: graph.KindMethod, Name: "Handle",
		FilePath: "repo/pkg2/Impl.cs", StartLine: 8, EndLine: 30}
	view := newLSPGraphView([]*graph.Node{iface, method}, nil)

	if got := view.matchNodeByFileLine("repo/pkg/IThing.cs", 10); got != iface {
		t.Fatalf("slash lookup vs backslash-spelled node: got %+v, want iface", got)
	}
	if got := view.matchCallableByFileLine(`repo/pkg2\Impl.cs`, 12); got != method {
		t.Fatalf("backslash lookup vs slash-spelled node: got %+v, want method", got)
	}
	if got := view.findDeclarationNode("repo/pkg/IThing.cs", 5, "IThing"); got != iface {
		t.Fatalf("findDeclarationNode across spellings: got %+v, want iface", got)
	}
}

func TestStorePathSpellingsCoverBothSeparators(t *testing.T) {
	got := storePathSpellings("repo", "a/b/C.cs")
	want := []string{"repo/a/b/C.cs", `repo/a\b\C.cs`}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("spellings: got %q, want %q", got, want)
	}
	if got := storePathSpellings("repo", "C.cs"); len(got) != 1 || got[0] != "repo/C.cs" {
		t.Fatalf("separator-free rel: got %q, want the single slash spelling", got)
	}
	if got := storePathSpellings("repo", ""); got != nil {
		t.Fatalf("empty rel: got %q, want nil", got)
	}
}

func TestConfirmRefMatchesSiteAcrossSeparatorSpellings(t *testing.T) {
	absRoot := t.TempDir()
	caller := &graph.Node{ID: "c1", Kind: graph.KindMethod, Name: "Caller",
		RepoPrefix: "repo", FilePath: `repo/Domain\Svc.cs`, StartLine: 30, EndLine: 60}
	edge := &graph.Edge{From: "c1", To: "t1", Kind: graph.EdgeCalls,
		FilePath: `repo/Domain\Svc.cs`, Line: 42}
	// The server answers with a URI, so the ref path comes back
	// slash-spelled; the edge site is backslash-spelled.
	ref := Location{
		URI:   pathToURI(filepath.Join(absRoot, "Domain", "Svc.cs")),
		Range: Range{Start: Position{Line: 41}},
	}

	p := &Provider{}
	if !p.confirmRefMatchesSite([]Location{ref}, absRoot, "repo",
		enrichTarget{node: caller, edge: edge}) {
		t.Fatal("reference at the site line must confirm across separator spellings")
	}
}
