package indexer

import "github.com/zzet/gortex/internal/graph"

// affectedTypeSet computes the type/interface IDs whose inferred
// implements/override edges a set of changed (stale) files can affect:
//   - every KindType / KindInterface the changed files define (their inferred
//     out/in edges were dropped on eviction), and
//   - the owning type of every KindMethod in a changed file (a method add/remove
//     changes the type's method-set, which can newly satisfy / break an
//     interface defined in an unchanged file).
//
// types is the full re-check set; ifaces is the subset of changed interfaces
// (every type must be re-checked against a changed interface). Re-checking
// every (type, interface) pair with an endpoint in these sets re-lands exactly
// the edges eviction dropped — add-parity with the full pass.
const affectedSetReadBatchSize = 256

func (idx *Indexer) affectedTypeSet(graphPaths []string) (types, ifaces map[string]bool) {
	types = map[string]bool{}
	ifaces = map[string]bool{}
	for start := 0; start < len(graphPaths); start += affectedSetReadBatchSize {
		end := start + affectedSetReadBatchSize
		if end > len(graphPaths) {
			end = len(graphPaths)
		}
		chunk := graphPaths[start:end]
		nodesByFile := idx.graph.GetFileNodesByPaths(chunk)
		var methodIDs []string
		for _, p := range chunk {
			for _, n := range nodesByFile[p] {
				if n == nil {
					continue
				}
				switch n.Kind {
				case graph.KindType, graph.KindInterface:
					types[n.ID] = true
					if n.Kind == graph.KindInterface {
						ifaces[n.ID] = true
					}
				case graph.KindMethod:
					methodIDs = append(methodIDs, n.ID)
				}
			}
		}
		if len(methodIDs) == 0 {
			continue
		}
		edgesByMethod := idx.graph.GetOutEdgesByNodeIDs(methodIDs)
		for _, methodID := range methodIDs {
			for _, e := range edgesByMethod[methodID] {
				if e != nil && e.Kind == graph.EdgeMemberOf {
					types[e.To] = true
				}
			}
		}
	}
	return types, ifaces
}

// staleFilesAffectDerivedEdges reports whether any stale file carries code
// structure (functions / methods / types / fields / …) that the capability
// and framework-dispatch synthesizers derive edges from. When every stale
// file is non-code — a README, a JSON/YAML config, a data file — those
// whole-graph passes cannot produce or change any edge, so the caller skips
// them (the doc/config-edit fast path). Sound by construction: a file with
// no structural nodes contributes no calls / reads / writes / dispatch
// sites, which is the only input those synthesizers read.
func (idx *Indexer) staleFilesAffectDerivedEdges(staleFiles []string) bool {
	graphPaths := idx.graphFilePaths(staleFiles)
	for start := 0; start < len(graphPaths); start += affectedSetReadBatchSize {
		end := start + affectedSetReadBatchSize
		if end > len(graphPaths) {
			end = len(graphPaths)
		}
		chunk := graphPaths[start:end]
		nodesByFile := idx.graph.GetFileNodesByPaths(chunk)
		for _, p := range chunk {
			for _, n := range nodesByFile[p] {
				if n != nil && isStructuralKind(n.Kind) {
					return true
				}
			}
		}
	}
	return false
}
