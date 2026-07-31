package indexer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
)

const (
	sourceSemanticFingerprintMeta = "source_semantic_fingerprint"
	sourceMetadataFingerprintMeta = "source_metadata_fingerprint"
	sourceCoreFingerprintMeta     = "source_core_fingerprint"
)

type fileDeltaFingerprints struct {
	semantic string
	metadata string
	core     string
}

// preparedExtraction is the parse result produced by the watcher's delta
// probe. A structural edit consumes it in indexFile so the same bytes are not
// parsed twice. src is the transformed source, matching indexFile's input to
// extractFile.
type preparedExtraction struct {
	absPath     string
	relPath     string
	lang        string
	src         []byte
	result      *parser.ExtractionResult
	readVersion fileReadVersion
}

// fileDeltaProbe exposes phase timings and the three delta boundaries used by
// the watcher: metadata-only, artifact-only, and semantic topology.
type fileDeltaProbe struct {
	fingerprints    fileDeltaFingerprints
	derived         derivedFingerprints
	read            time.Duration
	extract         time.Duration
	coverage        time.Duration
	fingerprintTime time.Duration
	readVersion     fileReadVersion
}

// prepareFileDelta parses the current file once and caches that exact
// extraction for either the bounded refresh or the structural reindex. Cold
// indexing deliberately does not pay this fingerprint cost: an old/missing
// fingerprint gets one conservative structural patch, which stamps the file
// for subsequent edits.
func (idx *Indexer) prepareFileDelta(filePath string) (fileDeltaProbe, bool) {
	var probe fileDeltaProbe
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return probe, false
	}
	relPath := idx.relKey(absPath)

	started := time.Now()
	src, readVersion, err := readFileWithVersion(absPath)
	probe.read = time.Since(started)
	if err != nil {
		return probe, false
	}
	lang, ok := idx.effectiveLanguage(absPath, src)
	if !ok {
		return probe, false
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return probe, false
	}
	if maxSize := idx.config.MaxFileSize; maxSize > 0 && int64(len(src)) > maxSize {
		return probe, false
	}
	if _, skip := idx.newContentAdmissionGate().skip(lang, int64(len(src))); skip {
		return probe, false
	}
	src = idx.transforms.run(relPath, src)

	var pool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		pool, quarantine = idx.sharedParsePool()
	}
	started = time.Now()
	result, skipped, err := idx.extractFile(pool, quarantine, absPath, relPath, lang, ext, src)
	probe.extract = time.Since(started)
	if quarantine != nil && quarantine.Len() > 0 {
		_ = quarantine.Save()
	}
	// Ownership of the result's tree-sitter tree — C memory the Go GC
	// cannot reclaim — passes to idx.prepared only on the success path
	// at the bottom. Every early return below drops the result on the
	// floor, so release the tree unless it was stored.
	stored := false
	defer func() {
		if !stored {
			result.ReleaseTree()
		}
	}()
	if result == nil || skipped || err != nil {
		return probe, false
	}

	started = time.Now()
	idx.applyCoverageDomains(relPath, lang, src, result)
	probe.coverage = time.Since(started)

	started = time.Now()
	fingerprints, derived, ok := extractionFingerprints(result)
	probe.fingerprintTime = time.Since(started)
	if !ok {
		return probe, false
	}
	probe.fingerprints = fingerprints
	probe.derived = derived
	stampExtractionGraphFingerprints(result, fingerprints)
	stampDerivedFingerprints(result, derived)

	idx.preparedMu.Lock()
	if idx.prepared == nil {
		idx.prepared = make(map[string]*preparedExtraction)
	}
	// A second prepare for the same path before the first was consumed
	// displaces the earlier entry; it owns a tree that nothing else can
	// reach once the map slot is overwritten.
	displaced := idx.prepared[absPath]
	idx.prepared[absPath] = &preparedExtraction{
		absPath:     absPath,
		relPath:     relPath,
		lang:        lang,
		src:         append([]byte(nil), src...),
		result:      result,
		readVersion: readVersion,
	}
	idx.preparedMu.Unlock()
	// Released outside the lock: Release closes the tree through cgo.
	displaced.releaseTree()
	stored = true
	probe.readVersion = readVersion
	return probe, true
}

// releaseTree drops the prepared extraction's parse tree. Nil-safe on
// the entry and on the result, so a caller that took a map slot which
// may or may not have been occupied can call it unconditionally.
func (p *preparedExtraction) releaseTree() {
	if p == nil {
		return
	}
	p.result.ReleaseTree()
}

func (idx *Indexer) takePreparedExtraction(absPath, relPath, lang string, src []byte) (*parser.ExtractionResult, bool) {
	idx.preparedMu.Lock()
	prepared := idx.prepared[absPath]
	delete(idx.prepared, absPath)
	idx.preparedMu.Unlock()
	if prepared == nil || prepared.relPath != relPath || prepared.lang != lang || !bytes.Equal(prepared.src, src) {
		// The entry is already out of the map and the caller gets
		// nothing, so this is the last reference to its tree.
		prepared.releaseTree()
		return nil, false
	}
	return prepared.result, true
}

// takePreparedSnapshot consumes the speculative parse without re-reading disk.
// Direct IndexFile uses this to commit the exact accepted read and let its
// original receipt report a concurrent write. Batch refreshes use
// takePreparedRefresh below, which replaces a stale speculative parse instead.
func (idx *Indexer) takePreparedSnapshot(filePath string) (*preparedExtraction, bool) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, false
	}
	idx.preparedMu.Lock()
	prepared := idx.prepared[absPath]
	delete(idx.prepared, absPath)
	idx.preparedMu.Unlock()
	return prepared, prepared != nil
}

func (idx *Indexer) takePreparedRefresh(filePath string) (*preparedExtraction, bool) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, false
	}
	prepared, ok := idx.takePreparedSnapshot(absPath)
	if !ok {
		return nil, false
	}
	current, readVersion, err := readFileWithVersion(absPath)
	if err != nil {
		prepared.releaseTree()
		return nil, false
	}
	current = idx.transforms.run(prepared.relPath, current)
	if !bytes.Equal(current, prepared.src) {
		// The file moved on since the speculative parse; the entry is
		// already out of the map, so drop its tree with it.
		prepared.releaseTree()
		return nil, false
	}
	prepared.readVersion = readVersion
	return prepared, true
}

func (idx *Indexer) discardPreparedExtraction(filePath string) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return
	}
	idx.preparedMu.Lock()
	discarded := idx.prepared[absPath]
	delete(idx.prepared, absPath)
	idx.preparedMu.Unlock()
	// Released outside the lock: Release closes the tree through cgo.
	discarded.releaseTree()
}

type fingerprintMode uint8

const (
	fingerprintMetadata fingerprintMode = iota
	fingerprintSemantic
	fingerprintCore
	fingerprintDerived
)

var presentationMetaKeys = map[string]struct{}{
	"body": {}, "comment": {}, "comments": {}, "doc": {},
	"documentation": {}, "search_doc": {}, "section_text": {},
	"snippet": {}, "source": {}, "source_text": {},
}

func isFingerprintMeta(key string) bool {
	switch key {
	case sourceSemanticFingerprintMeta, sourceMetadataFingerprintMeta, sourceCoreFingerprintMeta,
		sourceDerivedDeclFingerprintMeta, sourceDerivedImportFingerprintMeta,
		sourceDerivedRuntimeFingerprintMeta, sourceDerivedArtifactFingerprintMeta:
		return true
	default:
		return false
	}
}

func isArtifactNodeKind(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindArtifact, graph.KindDoc, graph.KindLicense, graph.KindRelease, graph.KindTeam, graph.KindTodo:
		return true
	default:
		return false
	}
}

func fingerprintMetaKeys(meta map[string]any, mode fingerprintMode, keepPresentation bool) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for key := range meta {
		if isFingerprintMeta(key) {
			continue
		}
		if mode != fingerprintMetadata && (!keepPresentation || mode == fingerprintDerived) {
			if _, presentation := presentationMetaKeys[key]; presentation {
				continue
			}
		}
		if mode == fingerprintDerived {
			switch strings.ToLower(key) {
			case "body", "body_hash", "body_text", "clone_sig", "content", "raw_source", "snippet", "source_text":
				continue
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// semanticFingerprintCoversDerived reports whether the semantic row digest is
// also a valid derived-work digest. Most extracted rows have no metadata, so
// reusing that digest removes a third full field/hash pass from cold indexing.
func semanticFingerprintCoversDerived(meta map[string]any, keepPresentation bool) bool {
	for key := range meta {
		if isFingerprintMeta(key) {
			continue
		}
		if _, presentation := presentationMetaKeys[key]; presentation {
			if keepPresentation {
				return false
			}
			continue
		}
		switch strings.ToLower(key) {
		case "body", "body_hash", "body_text", "clone_sig", "content", "raw_source", "snippet", "source_text":
			return false
		}
	}
	return true
}

type fingerprintDigest [sha256.Size]byte

func writeFingerprintUint(h hash.Hash, value uint64) {
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(scratch[:], value)
	_, _ = h.Write(scratch[:n])
}

func writeFingerprintInt(h hash.Hash, value int) {
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutVarint(scratch[:], int64(value))
	_, _ = h.Write(scratch[:n])
}

func writeFingerprintBytes(h hash.Hash, value []byte) {
	writeFingerprintUint(h, uint64(len(value)))
	_, _ = h.Write(value)
}

func writeFingerprintString(h hash.Hash, value string) {
	writeFingerprintUint(h, uint64(len(value)))
	_, _ = h.Write([]byte(value))
}

func writeFingerprintBool(h hash.Hash, value bool) {
	if value {
		_, _ = h.Write([]byte{1})
		return
	}
	_, _ = h.Write([]byte{0})
}

func writeFingerprintMeta(h hash.Hash, meta map[string]any, mode fingerprintMode, keepPresentation bool) bool {
	keys := fingerprintMetaKeys(meta, mode, keepPresentation)
	writeFingerprintUint(h, uint64(len(keys)))
	for _, key := range keys {
		encoded, err := json.Marshal(meta[key])
		if err != nil {
			return false
		}
		writeFingerprintString(h, key)
		writeFingerprintBytes(h, encoded)
	}
	return true
}

func nodeFingerprintDigest(h hash.Hash, node *graph.Node, mode fingerprintMode) (fingerprintDigest, bool) {
	h.Reset()
	_, _ = h.Write([]byte{'N'})
	writeFingerprintString(h, node.ID)
	writeFingerprintString(h, string(node.Kind))
	writeFingerprintString(h, node.Name)
	writeFingerprintString(h, node.QualName)
	writeFingerprintString(h, node.FilePath)
	if mode == fingerprintMetadata {
		writeFingerprintInt(h, node.StartLine)
		writeFingerprintInt(h, node.EndLine)
		writeFingerprintInt(h, node.StartColumn)
		writeFingerprintInt(h, node.EndColumn)
	} else {
		writeFingerprintInt(h, 0)
		writeFingerprintInt(h, 0)
		writeFingerprintInt(h, 0)
		writeFingerprintInt(h, 0)
	}
	writeFingerprintString(h, node.Language)
	if !writeFingerprintMeta(h, node.Meta, mode, isArtifactNodeKind(node.Kind)) {
		return fingerprintDigest{}, false
	}
	writeFingerprintString(h, node.RepoPrefix)
	writeFingerprintString(h, node.WorkspaceID)
	writeFingerprintString(h, node.ProjectID)
	writeFingerprintString(h, node.AbsoluteFilePath)
	writeFingerprintString(h, node.Origin)
	writeFingerprintBool(h, node.Stub)
	fetchedAt, err := node.FetchedAt.MarshalJSON()
	if err != nil {
		return fingerprintDigest{}, false
	}
	writeFingerprintBytes(h, fetchedAt)
	var digest fingerprintDigest
	h.Sum(digest[:0])
	return digest, true
}

func edgeFingerprintDigest(h hash.Hash, edge *graph.Edge, mode fingerprintMode) (fingerprintDigest, bool) {
	if math.IsNaN(edge.Confidence) || math.IsInf(edge.Confidence, 0) {
		return fingerprintDigest{}, false
	}
	h.Reset()
	_, _ = h.Write([]byte{'E'})
	writeFingerprintString(h, edge.From)
	writeFingerprintString(h, edge.To)
	writeFingerprintString(h, string(edge.Kind))
	writeFingerprintString(h, edge.FilePath)
	if mode == fingerprintMetadata {
		writeFingerprintInt(h, edge.Line)
	} else {
		writeFingerprintInt(h, 0)
	}
	writeFingerprintUint(h, math.Float64bits(edge.Confidence))
	writeFingerprintString(h, edge.ConfidenceLabel)
	writeFingerprintString(h, edge.Origin)
	writeFingerprintString(h, edge.Tier)
	writeFingerprintBool(h, edge.CrossRepo)
	writeFingerprintString(h, edge.Alias)
	if !writeFingerprintMeta(h, edge.Meta, mode, false) {
		return fingerprintDigest{}, false
	}
	var digest fingerprintDigest
	h.Sum(digest[:0])
	return digest, true
}

func stableFingerprintDigests(rows []fingerprintDigest) string {
	sort.Slice(rows, func(i, j int) bool {
		return bytes.Compare(rows[i][:], rows[j][:]) < 0
	})
	h := sha256.New()
	for _, row := range rows {
		_, _ = h.Write(row[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func extractionFingerprints(result *parser.ExtractionResult) (fileDeltaFingerprints, derivedFingerprints, bool) {
	if result == nil {
		return fileDeltaFingerprints{}, derivedFingerprints{}, false
	}
	var artifactIDs map[string]struct{}
	for _, node := range result.Nodes {
		if node == nil || !isArtifactNodeKind(node.Kind) {
			continue
		}
		if artifactIDs == nil {
			artifactIDs = make(map[string]struct{})
		}
		artifactIDs[node.ID] = struct{}{}
	}

	capacity := len(result.Nodes) + len(result.Edges)
	metadataRows := make([]fingerprintDigest, 0, capacity)
	semanticRows := make([]fingerprintDigest, 0, capacity)
	coreRows := make([]fingerprintDigest, 0, capacity)
	var declarations, imports, runtimeRows, artifacts []fingerprintDigest
	h := sha256.New()
	for _, node := range result.Nodes {
		if node == nil {
			continue
		}
		metadata, ok := nodeFingerprintDigest(h, node, fingerprintMetadata)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		semantic, ok := nodeFingerprintDigest(h, node, fingerprintSemantic)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		artifact := false
		if _, artifact = artifactIDs[node.ID]; !artifact {
			coreRows = append(coreRows, semantic)
		}
		derived := semantic
		if !semanticFingerprintCoversDerived(node.Meta, artifact) {
			derived, ok = nodeFingerprintDigest(h, node, fingerprintDerived)
			if !ok {
				return fileDeltaFingerprints{}, derivedFingerprints{}, false
			}
		}
		metadataRows = append(metadataRows, metadata)
		semanticRows = append(semanticRows, semantic)
		if isDeclarationNodeKind(node.Kind) {
			declarations = append(declarations, derived)
		}
		if isImportNodeKind(node.Kind) {
			imports = append(imports, derived)
		}
		if artifact {
			artifacts = append(artifacts, derived)
		}
	}
	for _, edge := range result.Edges {
		if edge == nil {
			continue
		}
		metadata, ok := edgeFingerprintDigest(h, edge, fingerprintMetadata)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		semantic, ok := edgeFingerprintDigest(h, edge, fingerprintSemantic)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		_, fromArtifact := artifactIDs[edge.From]
		_, toArtifact := artifactIDs[edge.To]
		if !fromArtifact && !toArtifact {
			coreRows = append(coreRows, semantic)
		}
		derived := semantic
		if !semanticFingerprintCoversDerived(edge.Meta, false) {
			derived, ok = edgeFingerprintDigest(h, edge, fingerprintDerived)
			if !ok {
				return fileDeltaFingerprints{}, derivedFingerprints{}, false
			}
		}
		metadataRows = append(metadataRows, metadata)
		semanticRows = append(semanticRows, semantic)
		if isDeclarationEdgeKind(edge.Kind) {
			declarations = append(declarations, derived)
		}
		if isImportEdgeKind(edge.Kind) {
			imports = append(imports, derived)
		}
		if isRuntimeDerivedEdgeKind(edge.Kind) {
			runtimeRows = append(runtimeRows, derived)
		}
		if fromArtifact || toArtifact {
			artifacts = append(artifacts, derived)
		}
	}
	return fileDeltaFingerprints{
			metadata: stableFingerprintDigests(metadataRows),
			semantic: stableFingerprintDigests(semanticRows),
			core:     stableFingerprintDigests(coreRows),
		}, derivedFingerprints{
			declarations: stableFingerprintDigests(declarations),
			imports:      stableFingerprintDigests(imports),
			runtime:      stableFingerprintDigests(runtimeRows),
			artifacts:    stableFingerprintDigests(artifacts),
		}, true
}

func stampExtractionGraphFingerprints(result *parser.ExtractionResult, fingerprints fileDeltaFingerprints) {
	for _, n := range result.Nodes {
		if n == nil || n.Kind != graph.KindFile {
			continue
		}
		if n.Meta == nil {
			n.Meta = make(map[string]any)
		}
		n.Meta[sourceSemanticFingerprintMeta] = fingerprints.semantic
		n.Meta[sourceMetadataFingerprintMeta] = fingerprints.metadata
		n.Meta[sourceCoreFingerprintMeta] = fingerprints.core
		return
	}
}

func stampExtractionGraphFingerprint(result *parser.ExtractionResult) string {
	fingerprints, derived, ok := extractionFingerprints(result)
	if !ok {
		return ""
	}
	stampExtractionGraphFingerprints(result, fingerprints)
	stampDerivedFingerprints(result, derived)
	return fingerprints.metadata
}

func storedExtractionGraphFingerprints(nodes []*graph.Node) fileDeltaFingerprints {
	for _, n := range nodes {
		if n == nil || n.Kind != graph.KindFile || n.Meta == nil {
			continue
		}
		semantic, _ := n.Meta[sourceSemanticFingerprintMeta].(string)
		metadata, _ := n.Meta[sourceMetadataFingerprintMeta].(string)
		core, _ := n.Meta[sourceCoreFingerprintMeta].(string)
		return fileDeltaFingerprints{semantic: semantic, metadata: metadata, core: core}
	}
	return fileDeltaFingerprints{}
}

func mergeRefreshMeta(oldMeta, freshMeta map[string]any) map[string]any {
	merged := make(map[string]any, len(oldMeta)+len(freshMeta))
	for key, value := range oldMeta {
		if isFingerprintMeta(key) {
			continue
		}
		if _, presentation := presentationMetaKeys[key]; presentation {
			continue
		}
		merged[key] = value
	}
	for key, value := range freshMeta {
		merged[key] = value
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

type edgeRefreshKey struct {
	from  string
	kind  graph.EdgeKind
	alias string
}

func metadataEdgeRefreshes(g graph.Store, graphPath string, priorNodes, freshNodes []*graph.Node, freshEdges []*graph.Edge) ([]graph.EdgeReindex, bool) {
	if len(freshNodes) == 0 {
		return nil, false
	}
	priorByID := make(map[string]*graph.Node, len(priorNodes))
	ids := make([]string, 0, len(priorNodes))
	for _, n := range priorNodes {
		if n == nil {
			continue
		}
		priorByID[n.ID] = n
		ids = append(ids, n.ID)
	}
	if len(priorByID) != len(freshNodes) {
		return nil, false
	}
	newIDs := make(map[string]struct{}, len(freshNodes))
	for _, n := range freshNodes {
		if n == nil || priorByID[n.ID] == nil {
			return nil, false
		}
		newIDs[n.ID] = struct{}{}
	}

	reuse, _ := captureIncrementalState(g, graphPath)
	applyResolvedOutEdges(g, freshEdges, reuse, newIDs)

	freshByKey := make(map[edgeRefreshKey][]*graph.Edge)
	for _, edge := range freshEdges {
		if edge == nil {
			continue
		}
		if _, local := priorByID[edge.From]; !local {
			return nil, false
		}
		key := edgeRefreshKey{from: edge.From, kind: edge.Kind, alias: edge.Alias}
		freshByKey[key] = append(freshByKey[key], edge)
	}
	oldByKey := make(map[edgeRefreshKey][]*graph.Edge)
	for _, edges := range graph.OutEdgesForNodes(g, ids) {
		for _, edge := range edges {
			if edge == nil {
				continue
			}
			key := edgeRefreshKey{from: edge.From, kind: edge.Kind, alias: edge.Alias}
			if _, needed := freshByKey[key]; needed {
				oldByKey[key] = append(oldByKey[key], edge)
			}
		}
	}

	updates := make([]graph.EdgeReindex, 0, len(freshEdges))
	for key, fresh := range freshByKey {
		old := oldByKey[key]
		if len(old) != len(fresh) {
			return nil, false
		}
		sort.Slice(old, func(i, j int) bool {
			if old[i].Line != old[j].Line {
				return old[i].Line < old[j].Line
			}
			return old[i].To < old[j].To
		})
		sort.Slice(fresh, func(i, j int) bool {
			if fresh[i].Line != fresh[j].Line {
				return fresh[i].Line < fresh[j].Line
			}
			return fresh[i].To < fresh[j].To
		})
		for i := range fresh {
			before := old[i]
			after := *before
			after.FilePath = fresh[i].FilePath
			after.Line = fresh[i].Line
			after.Alias = fresh[i].Alias
			after.Meta = mergeRefreshMeta(before.Meta, fresh[i].Meta)
			updates = append(updates, graph.EdgeReindex{
				Edge: &after, OldTo: before.To, OldFilePath: before.FilePath,
				OldLine: before.Line, RefreshIdentity: true,
			})
		}
	}
	return updates, true
}

// applyPreparedMetadataRefresh updates source-owned node metadata/locations and
// stable edge spans without evicting the file or invoking the resolver. A
// shape mismatch is conservative: the caller falls back to structural reindex.
func (idx *Indexer) applyPreparedMetadataRefresh(filePath string, priorNodes []*graph.Node) ([]*graph.Node, bool, bool) {
	prepared, ok := idx.takePreparedRefresh(filePath)
	if !ok || prepared.result == nil {
		return nil, false, false
	}
	result := prepared.result
	// takePreparedRefresh already removed the entry from idx.prepared,
	// so this function owns the tree. Only Nodes/Edges are read from
	// here on, and every shape-mismatch check below returns early —
	// release on all of them.
	defer result.ReleaseTree()
	idx.applyRepoPrefix(result.Nodes, result.Edges)
	graphPath := idx.prefixPath(prepared.relPath)

	priorByID := make(map[string]*graph.Node, len(priorNodes))
	for _, n := range priorNodes {
		if n != nil {
			priorByID[n.ID] = n
		}
	}
	if len(priorByID) != len(result.Nodes) {
		return nil, false, false
	}
	for i, fresh := range result.Nodes {
		if fresh == nil {
			return nil, false, false
		}
		old := priorByID[fresh.ID]
		if old == nil {
			return nil, false, false
		}
		cp := *fresh
		cp.Meta = mergeRefreshMeta(old.Meta, fresh.Meta)
		if cp.WorkspaceID == "" {
			cp.WorkspaceID = old.WorkspaceID
		}
		if cp.ProjectID == "" {
			cp.ProjectID = old.ProjectID
		}
		result.Nodes[i] = &cp
	}

	edgeUpdates, ok := metadataEdgeRefreshes(idx.graph, graphPath, priorNodes, result.Nodes, result.Edges)
	if !ok {
		return nil, false, false
	}
	if idx.contentSearcher() != nil && !idx.replaceContentSections(graphPath, result.Nodes, false) &&
		len(collectContentItems(result.Nodes)) > 0 {
		return nil, false, false
	}
	idx.graph.AddBatch(result.Nodes, nil)
	idx.graph.ReindexEdges(edgeUpdates)
	idx.persistFileMeta(prepared.relPath, prepared.src, result)

	batcher, _ := idx.graph.(graph.SymbolFTSBatchUpserter)
	var ftsItems []graph.SymbolFTSItem
	if batcher != nil {
		ftsItems = make([]graph.SymbolFTSItem, 0, len(result.Nodes))
	}
	for _, n := range result.Nodes {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		if idx.search != nil {
			idx.search.Remove(n.ID)
			idx.search.Add(n.ID, searchIndexFields(n, idx.projectName)...)
		}
		if batcher != nil {
			ftsItems = append(ftsItems, graph.SymbolFTSItem{
				NodeID: n.ID,
				Tokens: ftsTokensFor(n, idx.projectName),
			})
		}
	}
	if len(ftsItems) > 0 {
		if err := batcher.BatchUpsertSymbolFTS(ftsItems); err != nil {
			return nil, false, false
		}
	}
	fresh := idx.recordFileReadVersion(prepared.relPath, prepared.absPath, prepared.readVersion)
	return idx.graph.GetFileNodes(graphPath), true, fresh
}
