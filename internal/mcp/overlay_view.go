package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
)

// overlayViewCtxKey is the unexported context-key type for the
// per-request OverlaidView. The view is set by the tool-handler
// middleware (`wrapToolHandler`) and read by tool handlers via
// `s.readerFor(ctx)`. Unexported so external code can't smuggle a
// view onto an unrelated context.
type overlayViewCtxKey struct{}
type overlayRequestSnapshotCtxKey struct{}

// overlayRequestSnapshot is the immutable raw-buffer cohort used to build one
// request's OverlaidView. OverlayFile strings are immutable; retaining this
// slice through handler return adds only the copied slice headers and prevents
// a second manager snapshot from observing a different editor state.
type overlayRequestSnapshot struct {
	sessionID string
	workspace string
	files     []daemon.OverlayFile
}

func withOverlayRequestSnapshot(ctx context.Context, snapshot *overlayRequestSnapshot) context.Context {
	if ctx == nil || snapshot == nil {
		return ctx
	}
	return context.WithValue(ctx, overlayRequestSnapshotCtxKey{}, snapshot)
}

func overlayRequestSnapshotFromContext(ctx context.Context) (*overlayRequestSnapshot, bool) {
	if ctx == nil {
		return nil, false
	}
	snapshot, ok := ctx.Value(overlayRequestSnapshotCtxKey{}).(*overlayRequestSnapshot)
	return snapshot, ok && snapshot != nil
}

// WithOverlayView returns a child context carrying the
// shadow-graph view for the current `tools/call`. Tool handlers
// should not call this directly — `wrapToolHandler` is responsible
// for installing the view based on the calling session's overlay
// state.
func WithOverlayView(ctx context.Context, v *graph.OverlaidView) context.Context {
	if v == nil {
		return ctx
	}
	return context.WithValue(ctx, overlayViewCtxKey{}, v)
}

// OverlayViewFromContext returns the per-request view, or nil when
// the call isn't overlay-active. Tool handlers prefer
// `s.readerFor(ctx)` which folds this with the base graph; direct
// callers (the diff tool) use this when they need both base and
// overlay sides explicitly.
func OverlayViewFromContext(ctx context.Context) *graph.OverlaidView {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(overlayViewCtxKey{}).(*graph.OverlaidView); ok {
		return v
	}
	return nil
}

// readerFor returns the graph.Reader the calling tool handler should
// read through. When ctx carries an overlay view, that view is the
// reader and base is consulted through it. Otherwise the base graph
// is returned directly. The single helper keeps every read site
// overlay-aware with one line of plumbing.
//
// Never nil unless the server itself has no graph wired (test-only
// state). Callers that hot-loop on this should hoist the lookup —
// the helper is cheap but the indirection still matters at million-
// edge scales.
func (s *Server) readerFor(ctx context.Context) graph.Reader {
	if v := OverlayViewFromContext(ctx); v != nil {
		return v
	}
	return s.graph
}

// engineFor returns the query engine scoped to the calling request's
// graph reader. For non-overlay calls this is `s.engine` unchanged;
// for overlay-active calls the returned engine reads through the
// overlay view, so engine-level walks (`FindUsages`, `GetCallers`,
// `GetCallChain`, dependency / dependent walkers, hierarchy …) all
// see the editor-buffer state automatically.
//
// Cheap: WithReader is a shallow clone (one struct copy, shares
// search provider and rerank pipeline). Safe to call inside hot
// tool-handler paths.
func (s *Server) engineFor(ctx context.Context) *query.Engine {
	if s == nil || s.engine == nil {
		return nil
	}
	if v := OverlayViewFromContext(ctx); v != nil {
		return s.engine.WithReader(v)
	}
	return s.engine
}

// overlayLayerCacheEntry is one (sessionID, content-hash sum) bucket
// in s.overlayLayerCache. The hash is over (sorted) overlay files'
// (path, content, deleted, base_sha) tuples; identical pushes from a
// long sequence of tool calls hit the same entry and reuse the same
// parsed layer without re-running the per-language extractors.
type overlayLayerCacheEntry struct {
	hash  string
	layer *graph.OverlayLayer
	// Files captured in the entry, for invalidation lookups and for
	// the diff tool's enumeration.
	files []string
}

func (s *Server) snapshotOverlayRequestForCtx(ctx context.Context) (*overlayRequestSnapshot, error) {
	if s == nil || s.overlays == nil {
		return nil, nil
	}
	sessionID := SessionIDFromContext(ctx)
	if sessionID == "" {
		return nil, nil
	}
	workspace, files, err := s.overlays.SnapshotFor(sessionID)
	if err != nil {
		// Pin the attempted empty cohort. A nested facade must not retry and
		// accidentally observe buffers pushed after the outer request began.
		return &overlayRequestSnapshot{sessionID: sessionID}, nil
	}
	return &overlayRequestSnapshot{sessionID: sessionID, workspace: workspace, files: files}, nil
}

// prepareOverlayRequest pins one manager snapshot and the graph view derived
// from it onto the request context. Reusing an already-prepared context is
// idempotent, which keeps nested facade calls on the same editor-buffer cohort.
func (s *Server) prepareOverlayRequest(ctx context.Context) (context.Context, *graph.OverlaidView, error) {
	if ctx == nil {
		return ctx, nil, nil
	}
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if ok && snapshot.sessionID != SessionIDFromContext(ctx) {
		return ctx, nil, fmt.Errorf("overlay request snapshot belongs to session %q, not %q", snapshot.sessionID, SessionIDFromContext(ctx))
	}
	if view := OverlayViewFromContext(ctx); view != nil {
		return ctx, view, nil
	}
	if !ok {
		var err error
		snapshot, err = s.snapshotOverlayRequestForCtx(ctx)
		if err != nil {
			return ctx, nil, err
		}
		if snapshot != nil {
			ctx = withOverlayRequestSnapshot(ctx, snapshot)
		}
	}
	if snapshot == nil {
		return ctx, nil, nil
	}
	view, err := s.buildOverlayViewForCtx(ctx)
	if err != nil {
		return ctx, nil, err
	}
	if view != nil {
		ctx = WithOverlayView(ctx, view)
	}
	return ctx, view, nil
}

// validateOverlayRequestSnapshot ensures every raw buffer belongs to the
// workspace under which the session was registered. Unknown and foreign paths
// fail closed before parsing so a session cannot shadow another workspace.
func (s *Server) validateOverlayRequestSnapshot(snapshot *overlayRequestSnapshot) error {
	if snapshot == nil {
		return nil
	}
	for _, file := range snapshot.files {
		absPath, err := s.resolveOverlayAbsPath(file.Path)
		if err != nil {
			return err
		}
		owner := s.pickIndexerForPath(absPath)
		if absPath == "" || owner == nil {
			return fmt.Errorf("overlay path %q is outside the registered workspace", file.Path)
		}
		if snapshot.workspace != "" && owner.WorkspaceID() != snapshot.workspace {
			return fmt.Errorf("overlay path %q belongs to workspace %q, not registered workspace %q", file.Path, owner.WorkspaceID(), snapshot.workspace)
		}
	}
	return nil
}

// buildOverlayViewForCtx consumes only the raw snapshot installed by request
// preparation, ensuring parsing and later raw-buffer reads observe one cohort.
func (s *Server) buildOverlayViewForCtx(ctx context.Context) (*graph.OverlaidView, error) {
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || snapshot == nil || len(snapshot.files) == 0 {
		return nil, nil
	}
	if err := s.validateOverlayRequestSnapshot(snapshot); err != nil {
		return nil, err
	}
	files := snapshot.files
	sessID := SessionIDFromContext(ctx)

	// Drift check up front for every overlay that carries a BaseSHA.
	// We do it here, before parsing, so a stale overlay never costs
	// the extractor time and the client gets a clear error.
	for _, ov := range files {
		if ov.BaseSHA == "" {
			continue
		}
		abs, resolveErr := s.resolveOverlayAbsPath(ov.Path)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if abs == "" {
			continue
		}
		if !overlaySHAMatches(abs, ov.BaseSHA) {
			return nil, fmt.Errorf("%w: %s", daemon.ErrOverlayDrift, ov.Path)
		}
	}

	hash := hashOverlayFiles(files)

	// Cache hit: same session pushed the same buffers; reuse the
	// parsed layer. The cache stores up to one entry per session; a
	// changed content hash evicts the prior entry.
	if v, ok := s.overlayLayerCache.Load(sessID); ok {
		if entry := v.(*overlayLayerCacheEntry); entry.hash == hash {
			return graph.NewOverlaidView(s.graph, entry.layer), nil
		}
		s.overlayLayerCache.Delete(sessID)
	}

	// Cache miss: parse + resolve under a per-server build mutex so
	// two requests for the same fresh content don't duplicate the
	// extractor work. We re-check the cache under the lock.
	s.overlayLayerBuildMu.Lock()
	defer s.overlayLayerBuildMu.Unlock()
	if v, ok := s.overlayLayerCache.Load(sessID); ok {
		if entry := v.(*overlayLayerCacheEntry); entry.hash == hash {
			return graph.NewOverlaidView(s.graph, entry.layer), nil
		}
	}

	layer, paths, err := s.constructOverlayLayer(files)
	if err != nil {
		return nil, err
	}
	if layer == nil {
		return nil, nil
	}
	s.overlayLayerCache.Store(sessID, &overlayLayerCacheEntry{
		hash:  hash,
		layer: layer,
		files: paths,
	})
	return graph.NewOverlaidView(s.graph, layer), nil
}

// resolveOverlayAbsPath turns the overlay's caller-supplied path into
// the absolute filesystem path the indexer would have used. Accepts
// absolute paths, repo-prefixed paths (multi-repo), and repo-relative
// paths (single-repo). Returns ("", nil) for paths that don't
// resolve to any tracked workspace — those overlays are silently
// skipped, matching how the on-disk path treats untracked files.
func (s *Server) resolveOverlayAbsPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("overlay path is empty")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if s.multiIndexer != nil {
		if abs := s.multiIndexer.ResolveFilePath(p); abs != "" {
			return abs, nil
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			return filepath.Join(root, p), nil
		}
	}
	return "", nil
}

// resolveOverlayGraphPath turns the overlay path into the
// `graph_path` form (repo-prefixed in multi-repo mode, repo-relative
// in single-repo mode) — the form `GetFileNodes` and friends use.
func (s *Server) resolveOverlayGraphPath(p, absPath string) string {
	if s.multiIndexer != nil {
		if prefix := s.multiIndexer.RepoForFile(absPath); prefix != "" {
			if idx, _ := s.multiIndexer.IndexerForFile(absPath); idx != nil {
				if root := idx.RootPath(); root != "" {
					if rel, err := filepath.Rel(root, absPath); err == nil {
						return prefix + "/" + filepath.ToSlash(rel)
					}
				}
			}
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if rel, err := filepath.Rel(root, absPath); err == nil {
				return filepath.ToSlash(rel)
			}
		}
	}
	// Fall back to caller-supplied path — this is the single-repo
	// repo-relative case where p is already the graph_path.
	return filepath.ToSlash(p)
}

// pickIndexerForPath chooses the per-repo Indexer (multi-repo) or
// the single Indexer (single-repo) that owns absPath. Returns nil
// when no Indexer owns the path (the overlay is silently skipped
// upstream).
func (s *Server) pickIndexerForPath(absPath string) *indexer.Indexer {
	if s.multiIndexer != nil {
		if idx, _ := s.multiIndexer.IndexerForFile(absPath); idx != nil {
			return idx
		}
	}
	if s.indexer != nil {
		if root := s.indexer.RootPath(); root != "" {
			if rel, err := filepath.Rel(root, absPath); err == nil && !strings.HasPrefix(rel, "..") {
				return s.indexer
			}
		}
	}
	return nil
}

// constructOverlayLayer is the parse-and-resolve pass. For each
// overlay file:
//
//   - Resolve the absolute and graph-prefixed paths.
//   - For deleted: true overlays, mark the graph path as a tombstone
//     and continue (no parsing).
//   - For content overlays, look up the per-language extractor via
//     the indexer's parser.Registry and call Extract on the buffer.
//   - Apply the repo prefix to the result so IDs match base's
//     `<repo-prefix>/<file>::<symbol>` shape.
//   - Add the parsed nodes + edges to the layer.
//   - Track removed-by-overlay symbols (names that existed in base's
//     view of the file but disappeared) so FindNodesByName drops
//     those base hits.
//
// After parsing all files, run a local resolver pass that rewrites
// every `unresolved::*` edge in the overlay to point at a real node
// when one exists in `(layer ∪ base)`. The pass is conservative —
// simple name resolution — but covers the common cases: direct
// function calls, method calls in the same file, intra-package
// references.
func (s *Server) constructOverlayLayer(files []daemon.OverlayFile) (*graph.OverlayLayer, []string, error) {
	if s.graph == nil {
		return nil, nil, nil
	}
	layer := graph.NewOverlayLayer()
	var coveredPaths []string

	for _, ov := range files {
		absPath, err := s.resolveOverlayAbsPath(ov.Path)
		if err != nil {
			return nil, nil, err
		}
		if absPath == "" {
			continue // untracked file — silent skip, matches disk path
		}
		graphPath := s.resolveOverlayGraphPath(ov.Path, absPath)
		coveredPaths = append(coveredPaths, graphPath)

		if ov.Deleted {
			// Tombstone: hide every base node for this file.
			for _, n := range s.graph.GetFileNodes(graphPath) {
				layer.MarkRemoved(n.Name, n.ID)
			}
			layer.MarkFile(graphPath, true)
			continue
		}

		idx := s.pickIndexerForPath(absPath)
		if idx == nil {
			continue
		}
		reg := idx.Registry()
		if reg == nil {
			continue
		}
		lang, ok := reg.DetectLanguageContent(absPath, []byte(ov.Content))
		if !ok {
			continue
		}
		ext, _ := reg.GetByLanguage(lang)
		if ext == nil {
			continue
		}
		root := idx.RootPath()
		relPath := graphPath
		if idx.RepoPrefix() != "" {
			relPath = strings.TrimPrefix(graphPath, idx.RepoPrefix()+"/")
		} else if root != "" {
			if r, err := filepath.Rel(root, absPath); err == nil {
				relPath = filepath.ToSlash(r)
			}
		}
		result, err := ext.Extract(relPath, []byte(ov.Content))
		if err != nil {
			return nil, nil, fmt.Errorf("overlay parse %s: %w", ov.Path, err)
		}
		// Track which base IDs disappear under the overlay so
		// FindNodesByName / GetInEdges filter them. Build the set
		// first from base, then mark every base ID that the overlay
		// did NOT re-emit (by ID equality).
		baseIDsByName := map[string]map[string]bool{}
		for _, n := range s.graph.GetFileNodes(graphPath) {
			set, ok := baseIDsByName[n.Name]
			if !ok {
				set = make(map[string]bool)
				baseIDsByName[n.Name] = set
			}
			set[n.ID] = true
		}
		overlayIDsByName := map[string]map[string]bool{}
		applyRepoPrefixToResult(result, idx.RepoPrefix())
		layer.MarkFile(graphPath, false)
		for _, n := range result.Nodes {
			layer.AddNode(graphPath, n)
			set, ok := overlayIDsByName[n.Name]
			if !ok {
				set = make(map[string]bool)
				overlayIDsByName[n.Name] = set
			}
			set[n.ID] = true
		}
		for _, e := range result.Edges {
			layer.AddEdge(e)
		}
		// Names that existed in base but were not re-emitted by the
		// overlay (same name, different ID, or absent entirely) get
		// marked removed so FindNodesByName filters the base hits.
		for name, baseIDs := range baseIDsByName {
			overlayIDs := overlayIDsByName[name]
			for id := range baseIDs {
				if !overlayIDs[id] {
					layer.MarkRemoved(name, id)
				}
			}
		}
	}

	if len(coveredPaths) == 0 {
		return nil, nil, nil
	}
	sort.Strings(coveredPaths)

	// Local resolver pass: rewrite unresolved overlay edges to point
	// at concrete IDs whenever a single best match exists in
	// (overlay ∪ base).
	s.resolveOverlayEdges(layer)

	return layer, coveredPaths, nil
}

// applyRepoPrefixToResult prepends repoPrefix to every node/edge in
// an extraction result so IDs match base's shape in multi-repo mode.
// Mirrors `Indexer.applyRepoPrefix` but kept here to avoid having to
// expose that helper to non-indexer packages.
func applyRepoPrefixToResult(result *parser.ExtractionResult, repoPrefix string) {
	if result == nil || repoPrefix == "" {
		return
	}
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		n.ID = repoPrefix + "/" + n.ID
		if n.FilePath != "" {
			n.FilePath = repoPrefix + "/" + n.FilePath
		}
		n.RepoPrefix = repoPrefix
	}
	for _, e := range result.Edges {
		if e == nil {
			continue
		}
		e.From = repoPrefix + "/" + e.From
		if !strings.HasPrefix(e.To, unresolvedPrefix) {
			e.To = repoPrefix + "/" + e.To
		}
	}
}

// unresolvedPrefix matches the resolver's prefix for placeholder
// edge targets. Kept as a package constant so resolveOverlayEdges
// can recognise placeholders without importing the resolver package
// (which would create a layering cycle through the indexer).
const unresolvedPrefix = "unresolved::"

// resolveOverlayEdges runs a conservative local resolver pass over
// the overlay layer's edges. For each placeholder `unresolved::*`
// edge:
//
//   - Strip the `unresolved::<kind>::` prefix to recover the target
//     name (and optional fully-qualified suffix).
//   - Look the name up in (layer.nodesByName ∪ base.FindNodesByName).
//     Prefer overlay matches over base; prefer a single unambiguous
//     match over multiple.
//   - On a unique match, rewrite the edge's To and re-index it in
//     the layer's outEdges / inEdges maps.
//
// The pass deliberately does NOT replicate the full resolver
// (interface dispatch, import-path attribution, dataflow, etc.) —
// overlay buffers are transient and the common case the editor
// cares about is "I added a call to Foo; does find_usages of Foo
// now include this site?" Direct name resolution covers that.
func (s *Server) resolveOverlayEdges(layer *graph.OverlayLayer) {
	if layer == nil {
		return
	}
	// Collect every From → []Edge that the layer holds. We iterate
	// over a copy of the map so we can rewrite layer edges
	// in-place via AddEdge / removal pattern (layer is meant
	// to be append-only post-construction; the resolver pass runs
	// before the layer is handed to the View, so we still own it).
	for _, edges := range layer.OutEdgesByFromAll() {
		for _, e := range edges {
			if !strings.HasPrefix(e.To, unresolvedPrefix) {
				continue
			}
			target := strings.TrimPrefix(e.To, unresolvedPrefix)
			// Strip kind segment if present (e.g. "call::FooBar").
			if i := strings.Index(target, "::"); i > 0 {
				target = target[i+2:]
			}
			// Strip trailing argument-count / disambiguator hints.
			if i := strings.Index(target, "@"); i > 0 {
				target = target[:i]
			}
			if target == "" {
				continue
			}
			resolved := s.lookupOverlayTarget(layer, target)
			if resolved == "" {
				continue
			}
			e.To = resolved
		}
	}
	// Rebuild the layer's inEdges index now that targets may have
	// changed. The layer exposes a Rebuild helper so we don't have
	// to know the internal map shape.
	layer.RebuildInEdges()
}

// lookupOverlayTarget tries to find a unique node with the given
// short name in (layer ∪ base). Returns the node ID on a unique
// match, empty string otherwise. Tied matches return empty so the
// edge stays as a placeholder rather than picking the wrong target.
func (s *Server) lookupOverlayTarget(layer *graph.OverlayLayer, name string) string {
	overlay := layer.NodesByName(name)
	if len(overlay) == 1 {
		return overlay[0].ID
	}
	if len(overlay) > 1 {
		return ""
	}
	if s.graph == nil {
		return ""
	}
	hits := s.graph.FindNodesByName(name)
	// Drop hits whose file is overlaid AND whose ID wasn't kept by
	// the overlay — those are now-deleted symbols.
	keep := hits[:0:0]
	for _, n := range hits {
		if layer.HasFile(graph.IDFile(n.ID)) {
			if !layer.HasNode(n.ID) {
				continue
			}
		}
		keep = append(keep, n)
	}
	if len(keep) == 1 {
		return keep[0].ID
	}
	return ""
}

// hashOverlayFiles produces a stable content-hash of an overlay
// file set so the cache can detect "same set, reuse parse".
func hashOverlayFiles(files []daemon.OverlayFile) string {
	sorted := make([]daemon.OverlayFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%t\x00%s\x00", f.Path, f.Deleted, f.BaseSHA)
		_, _ = h.Write([]byte(f.Content))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// overlayContentFor returns the editor-buffer content for an
// absolute path in the calling session's overlay, if any. Used by
// source-reading handlers (get_symbol_source, get_editing_context,
// smart_context) to substitute the overlay's text for the on-disk
// file when the request is overlay-active. Returns (content, true)
// on a hit, ("", false) otherwise — including the deleted-overlay
// case, since a tombstone has no content to return and callers
// should treat the file as absent.
func (s *Server) overlayContentFor(ctx context.Context, absPath string) (string, bool) {
	if s == nil || ctx == nil {
		return "", false
	}
	snapshot, ok := overlayRequestSnapshotFromContext(ctx)
	if !ok || snapshot == nil {
		return "", false
	}
	cleanedAbs := filepath.Clean(absPath)
	for _, ov := range snapshot.files {
		if ov.Deleted {
			continue
		}
		ovAbs, _ := s.resolveOverlayAbsPath(ov.Path)
		if ovAbs == "" {
			continue
		}
		if filepath.Clean(ovAbs) == cleanedAbs {
			return ov.Content, true
		}
	}
	return "", false
}

// overlayShadowsGraphPath reports whether the active request replaces or
// tombstones the path represented by a base search hit. Search indexes are
// built from durable files, so their snippets must never authenticate stale
// content from a file whose editor overlay owns the request-time view.
func (s *Server) overlayShadowsGraphPath(ctx context.Context, graphPath, nodeID string) bool {
	view := OverlayViewFromContext(ctx)
	if view == nil || view.Layer() == nil {
		return false
	}
	layer := view.Layer()
	for _, candidate := range []string{graphPath, graph.IDFile(nodeID)} {
		candidate = filepath.ToSlash(filepath.Clean(strings.TrimSpace(candidate)))
		if candidate != "" && candidate != "." && layer.HasFile(candidate) {
			return true
		}
	}
	return false
}

func (s *Server) filterOverlayContentHits(ctx context.Context, hits []graph.ContentHit) []graph.ContentHit {
	if OverlayViewFromContext(ctx) == nil || len(hits) == 0 {
		return hits
	}
	filtered := hits[:0:0]
	for _, hit := range hits {
		if !s.overlayShadowsGraphPath(ctx, hit.FilePath, hit.NodeID) {
			filtered = append(filtered, hit)
		}
	}
	return filtered
}

// overlayCacheInvalidate drops the cached layer for a session. Called
// by overlay_push / overlay_delete / overlay_drop so the next tool
// call re-parses with the fresh buffer state.
func (s *Server) overlayCacheInvalidate(sessID string) {
	if s == nil || sessID == "" {
		return
	}
	s.overlayLayerCache.Delete(sessID)
}

// Compile-time sanity: a sync.Mutex usage placeholder so future
// linter-driven import pruning doesn't strip the package.
var _ sync.Mutex
