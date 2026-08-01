package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// UIKit directory-convention name-resolution. Like the SwiftUI pass, this is a
// fallback for the references sourcekit-lsp leaves unresolved: a `*ViewController`
// binds to /ViewControllers/ or /Controllers/, a `*Cell` to /Cells/,
// /TableViewCells/ or /CollectionViewCells/, and a `*Delegate` / `*DataSource`
// to a /Delegates/ or /DataSources/ directory (or, via the same-dir tier, the
// owning controller's own directory). Swift and Objective-C references are
// considered.

var (
	uikitVCDirs       = []string{"/ViewControllers/", "/Controllers/", "/ViewController/", "/Controller/"}
	uikitCellDirs     = []string{"/Cells/", "/TableViewCells/", "/CollectionViewCells/", "/Cell/"}
	uikitDelegateDirs = []string{"/Delegates/", "/DataSources/", "/Delegate/", "/DataSource/"}
)

// ResolveUIKitRefs binds residual unresolved UIKit references to their
// directory-located definitions. Returns the count bound.
func ResolveUIKitRefs(g graph.Store) int {
	if g == nil {
		return 0
	}

	// The candidate census deliberately projects no edge metadata. SQLite keeps
	// metadata BLOBs on disk while we reject the overwhelming majority of edges.
	// Scan each kind separately to preserve the legacy pass order exactly, and
	// finish every cursor before point-reading nodes or writing rewritten edges.
	var candidateIDs []graph.EdgeIdentity
	var sourceIDs []string
	for _, kind := range []graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls} {
		for edge := range graph.EdgesLightSeq(g, kind) {
			if !isUIKitCandidateEdge(edge) {
				continue
			}
			candidateIDs = append(candidateIDs, graph.EdgeIdentityFor(edge))
			sourceIDs = append(sourceIDs, edge.From)
		}
	}
	if len(candidateIDs) == 0 {
		return 0
	}

	placements := graph.NodePlacementsByIDs(g, dedupeFrameworkIDs(sourceIDs))
	appleCandidates := candidateIDs[:0]
	for _, identity := range candidateIDs {
		if placement, ok := placements[identity.From]; ok && isAppleSourceFile(placement.FilePath) {
			appleCandidates = append(appleCandidates, identity)
		}
	}
	if len(appleCandidates) == 0 {
		return 0
	}

	// Refetch the exact surviving logical edges only after every scan cursor has
	// closed. This preserves metadata and provenance without retaining full Edge
	// values for the census.
	current := findFrameworkEdgesByIdentities(g, appleCandidates)
	resolved := 0
	var reindex []graph.EdgeReindex
	for _, identity := range appleCandidates {
		edge := current[identity]
		if !isUIKitCandidateEdge(edge) {
			continue
		}
		name := graph.UnresolvedName(edge.To)
		dirs, _ := uikitDirsFor(name)
		fromFile := placements[edge.From].FilePath
		targetID, conf := ResolveByConvention(g, name, "", dirs, fromFile)
		if targetID == "" {
			continue
		}
		oldTo := edge.To
		edge.To = targetID
		edge.Origin = graph.OriginASTInferred
		edge.Confidence = conf
		edge.ConfidenceLabel = graph.ConfidenceLabelFor(edge.Kind, conf)
		StampSynthesized(edge, SynthUIKitResolve)
		reindex = append(reindex, graph.EdgeReindex{Edge: edge, OldTo: oldTo})
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func isUIKitCandidateEdge(edge *graph.Edge) bool {
	if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
		return false
	}
	name := graph.UnresolvedName(edge.To)
	if name == "" || strings.ContainsRune(name, '.') {
		return false
	}
	_, ok := uikitDirsFor(name)
	return ok
}

// uikitDirsFor classifies a UIKit reference name into its convention dirs.
func uikitDirsFor(name string) ([]string, bool) {
	switch {
	case strings.HasSuffix(name, "ViewController"):
		return uikitVCDirs, true
	case strings.HasSuffix(name, "Cell"):
		return uikitCellDirs, true
	case strings.HasSuffix(name, "Delegate"), strings.HasSuffix(name, "DataSource"):
		return uikitDelegateDirs, true
	}
	return nil, false
}

// isAppleSourceFile reports whether a path is a Swift or Objective-C source
// file — the only files the UIKit pass binds references from.
func isAppleSourceFile(p string) bool {
	switch {
	case strings.HasSuffix(p, ".swift"), strings.HasSuffix(p, ".m"),
		strings.HasSuffix(p, ".mm"), strings.HasSuffix(p, ".h"):
		return true
	}
	return false
}
