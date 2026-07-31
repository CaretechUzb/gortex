package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// sveltekitLoadVia tags the synthesized page→server-load edges.
const sveltekitLoadVia = "sveltekit_load"

// ResolveSvelteKitLoad pairs a SvelteKit route's page with the server load
// module that feeds it: `+page.svelte` / `+page.ts` and the `+page.server.ts`
// in the same route directory share data through `load()`. This emits a
// tier-tagged edge from the page component to the server load function (or the
// server module file when no explicit `load` is found), so a trace from the
// rendered page reaches its server-side data source — a cross-file dependency a
// file-by-file scanner never connects. Repo-scoped via sameDispatchBoundary.
func ResolveSvelteKitLoad(g graph.Store) int {
	if g == nil {
		return 0
	}

	// Per route dir: the page node and the server module file, plus the load
	// function declared in the server module.
	type routeDir struct {
		pageNode      *graph.Node
		serverFile    graph.FileNodeIdentity
		hasServerFile bool
		serverLoadID  string
	}
	dirs := map[string]*routeDir{}
	get := func(dir string) *routeDir {
		if dirs[dir] == nil {
			dirs[dir] = &routeDir{}
		}
		return dirs[dir]
	}

	// First pass over file + component nodes to populate the page / server file.
	for file := range graph.FileNodeIdentitiesSeq(g, nil) {
		base, dir := pathBaseDir(file.FilePath)
		if isSvelteKitServerFile(base) {
			route := get(dir)
			route.serverFile = file
			route.hasServerFile = true
		}
	}
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindFunction} {
		for n := range g.NodesByKind(kind) {
			if n == nil {
				continue
			}
			base, dir := pathBaseDir(n.FilePath)
			switch {
			case kind == graph.KindType && isSvelteKitPageFile(base):
				// The component node (Meta component=true) is the page.
				if c, _ := n.Meta["component"].(bool); c {
					get(dir).pageNode = n
				}
			case kind == graph.KindFunction && isSvelteKitServerFile(base) && n.Name == "load":
				get(dir).serverLoadID = n.ID
			}
		}
	}

	dirNames := make([]string, 0, len(dirs))
	for d := range dirs {
		dirNames = append(dirNames, d)
	}
	sort.Strings(dirNames)

	var batch []*graph.Edge
	for _, d := range dirNames {
		rd := dirs[d]
		if rd.pageNode == nil {
			continue
		}
		// Prefer the explicit load function; fall back to the server module file.
		to := rd.serverLoadID
		sameBoundary := false
		if to != "" {
			toNode := g.GetNode(to)
			sameBoundary = sameDispatchBoundary(rd.pageNode, toNode)
		} else if rd.hasServerFile {
			to = rd.serverFile.ID
			sameBoundary = rd.pageNode.WorkspaceID == rd.serverFile.WorkspaceID
		}
		if to == "" || !sameBoundary {
			continue
		}
		batch = append(batch, &graph.Edge{
			From:            rd.pageNode.ID,
			To:              to,
			Kind:            graph.EdgeReferences,
			FilePath:        rd.pageNode.FilePath,
			Line:            rd.pageNode.StartLine,
			Confidence:      0.6,
			ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeReferences, 0.6),
			Origin:          graph.OriginASTInferred,
			Meta: map[string]any{
				"via":             sveltekitLoadVia,
				MetaSynthesizedBy: SynthSvelteKitLoad,
				MetaProvenance:    ProvenanceHeuristic,
			},
		})
	}
	for _, e := range batch {
		g.AddEdge(e)
	}
	return len(batch)
}

// pathBaseDir splits a slash path into its basename and directory.
func pathBaseDir(p string) (base, dir string) {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:], p[:i]
	}
	return p, ""
}

// isSvelteKitPageFile reports whether a basename is a SvelteKit page entry.
func isSvelteKitPageFile(base string) bool {
	for _, p := range []string{"+page.svelte", "+page.ts", "+page.js"} {
		if base == p {
			return true
		}
	}
	return false
}

// isSvelteKitServerFile reports whether a basename is a SvelteKit server module.
func isSvelteKitServerFile(base string) bool {
	for _, p := range []string{"+page.server.ts", "+page.server.js", "+layout.server.ts", "+layout.server.js"} {
		if base == p {
			return true
		}
	}
	return false
}
