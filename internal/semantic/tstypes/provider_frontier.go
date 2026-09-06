package tstypes

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// frontierFileKeys normalises one caller's file frontier into the graph file
// keys languageFiles yields, so the intersection below cannot silently miss a
// file because two layers spell the same path differently.
//
// The production dispatcher already hands over exact graph keys: the indexer's
// deferred-enrichment ledger stores prefixPath(relKey) forms
// (Indexer.markPendingEnrichFiles ← DerivedInvalidationPlan.Files /
// graphFilePaths), deferredEnrichFrontiers partitions those same keys by
// language, and Manager.EnrichFilesContext only dedups and sorts them. That
// identity case is the one that matters; the two other shapes cost nothing to
// accept and are what a test or an editor caller naturally produces — a path
// relative to repoRoot, and an absolute path under it.
//
// A repo-relative path is registered under BOTH its bare and its prefixed form,
// because the two are genuinely ambiguous in multi-repo mode: for repo prefix
// "local", "local/x.py" is a valid graph key AND a valid repo-relative path in
// a checkout that happens to have a top-level local/ directory. Registering
// both never loses a file. The extra candidate is inert: this set is only ever
// intersected with real graph file keys, so a candidate naming nothing selects
// nothing.
//
// A path escaping repoRoot is dropped rather than guessed at — the whole point
// of the repoPrefix gate in languageFiles is that a foreign repo's file must
// never be read against this repo's root.
func frontierFileKeys(repoPrefix, repoRoot string, filePaths []string) map[string]struct{} {
	keys := make(map[string]struct{}, 2*len(filePaths))
	add := func(key string) {
		if key == "" || key == "." {
			return
		}
		keys[key] = struct{}{}
		if repoPrefix != "" {
			keys[repoPrefix+"/"+key] = struct{}{}
		}
	}
	for _, filePath := range filePaths {
		if filePath == "" {
			continue
		}
		if filepath.IsAbs(filePath) {
			if repoRoot == "" {
				continue
			}
			rel, err := filepath.Rel(repoRoot, filePath)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if rel == ".." || strings.HasPrefix(rel, "../") {
				continue
			}
			add(rel)
			continue
		}
		add(filepath.ToSlash(filePath))
	}
	return keys
}

// frontierFiles is languageFiles restricted to the caller's frontier: the
// intersection, never a union and never a fallback.
//
// The selection deliberately runs the FULL languageFiles walk and filters its
// output rather than statting the frontier paths directly. Everything that walk
// enforces — the node's own RepoPrefix matching the repo being enriched (a
// path collision between two tracked repos otherwise reads the wrong repo's
// bytes), the vendored/generated low-value gate, the grammar-language gate, the
// on-disk existence and size cap — has to hold identically for a frontier pass,
// and duplicating those rules here is how they would drift apart.
func frontierFiles(g graph.Store, spec *LangSpec, repoPrefix, repoRoot string, filePaths []string) []fileRef {
	keys := frontierFileKeys(repoPrefix, repoRoot, filePaths)
	if len(keys) == 0 {
		return nil
	}
	candidates := languageFiles(g, spec, repoPrefix, repoRoot)
	out := make([]fileRef, 0, min(len(candidates), len(keys)))
	for _, ref := range candidates {
		if ref.node == nil {
			continue
		}
		if _, wanted := keys[ref.node.FilePath]; wanted {
			out = append(out, ref)
		}
	}
	return out
}

// tstypesFileNodeKindSet is tstypesFileNodeKinds as a lookup set, so the
// coverage denominator can be counted over exactly the node population the
// numerator (applier.coveredSymbols, fed by loadPageFileNodes) is drawn from.
// A denominator over a wider kind set would make a complete frontier pass
// report less than full coverage for no reason a caller could act on.
var tstypesFileNodeKindSet = func() map[graph.NodeKind]bool {
	set := make(map[graph.NodeKind]bool, len(tstypesFileNodeKinds))
	for _, kind := range tstypesFileNodeKinds {
		set[kind] = true
	}
	return set
}()

// countSymbolsInFiles is the frontier's coverage denominator: how many symbols
// of this provider's languages live in the files the pass was given. It is the
// file-scoped answer to the whole-repo pass's
// RepoLanguageSymbolCounter.CountRepoLanguageSymbols.
//
// The frontier is bounded by construction (a save's handful of files, or a
// worktree copy's divergence), so this is a small scoped projection rather than
// a repository scan — it takes the same NodesInFilesByKind capability ladder
// applier.loadPageFileNodes takes, and degrades to one repo-scoped walk only
// for compatibility stores that implement neither.
func (p *Provider) countSymbolsInFiles(g graph.Store, repoPrefix string, files []fileRef) int {
	if len(files) == 0 {
		return 0
	}
	langs := make(map[string]bool, len(p.spec.Languages))
	for _, language := range p.spec.Languages {
		langs[language] = true
	}
	paths := make([]string, 0, len(files))
	pathSet := make(map[string]struct{}, len(files))
	for _, ref := range files {
		if ref.node == nil || ref.node.FilePath == "" {
			continue
		}
		if _, duplicate := pathSet[ref.node.FilePath]; duplicate {
			continue
		}
		pathSet[ref.node.FilePath] = struct{}{}
		paths = append(paths, ref.node.FilePath)
	}
	if len(paths) == 0 {
		return 0
	}
	count := 0
	countNode := func(node *graph.Node) {
		if node == nil || !langs[node.Language] || !tstypesFileNodeKindSet[node.Kind] {
			return
		}
		count++
	}
	if streamer, ok := g.(graph.NodesInFilesByKindStreamer); ok {
		for _, group := range streamer.NodesInFilesByKindSeq(paths, tstypesFileNodeKinds) {
			for _, node := range group {
				countNode(node)
			}
		}
		return count
	}
	if finder, ok := g.(graph.NodesInFilesByKindFinder); ok {
		for _, node := range finder.NodesInFilesByKind(paths, tstypesFileNodeKinds) {
			countNode(node)
		}
		return count
	}
	for _, node := range g.GetRepoNodes(repoPrefix) {
		if node == nil {
			continue
		}
		if _, wanted := pathSet[node.FilePath]; !wanted {
			continue
		}
		countNode(node)
	}
	return count
}
