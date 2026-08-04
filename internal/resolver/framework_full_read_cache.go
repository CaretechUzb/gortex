package resolver

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// The cold framework loop may reuse decoded node projections, but it must
	// not become another unbounded whole-graph cache. This budget is temporary
	// (the cache dies with one framework settle) and includes unique node
	// payloads, query-result pointer slices, and the compact member projection.
	frameworkFullReadCacheRowCap  = 262144
	frameworkFullReadCacheByteCap = 128 << 20
)

type FrameworkFullReadCacheStats struct {
	NodeHits      int `json:"node_hits"`
	NodeMisses    int `json:"node_misses"`
	MemberHits    int `json:"member_hits"`
	MemberMisses  int `json:"member_misses"`
	Bypasses      int `json:"bypasses"`
	RetainedRows  int `json:"retained_rows"`
	RetainedBytes int `json:"retained_bytes"`
	RowCap        int `json:"row_cap"`
	ByteCap       int `json:"byte_cap"`
}

// frameworkFullReadCache owns immutable node-side projections shared by the
// serial cold/full framework synthesizers. Framework passes mutate edges, not
// declarations; the batching facade invalidates this cache if a future pass
// does mutate nodes or member_of relationships.
type frameworkFullReadCache struct {
	rowCap  int
	byteCap int

	nodeQueries map[string][]*graph.Node
	nodesByID   map[string]*graph.Node
	nodeRows    int
	nodeBytes   int
	queryBytes  int

	memberMethods      map[string][]graph.MemberMethodInfo
	memberMethodsReady bool
	memberRows         int
	memberBytes        int

	nodeHits     int
	nodeMisses   int
	memberHits   int
	memberMisses int
	bypasses     int
}

func newFrameworkFullReadCache() *frameworkFullReadCache {
	return newFrameworkFullReadCacheWithLimits(
		frameworkFullReadCacheRowCap,
		frameworkFullReadCacheByteCap,
	)
}

func newFrameworkFullReadCacheWithLimits(rowCap, byteCap int) *frameworkFullReadCache {
	return &frameworkFullReadCache{
		rowCap:      rowCap,
		byteCap:     byteCap,
		nodeQueries: make(map[string][]*graph.Node),
		nodesByID:   make(map[string]*graph.Node),
	}
}

func (c *frameworkFullReadCache) nodesByKinds(
	kinds []graph.NodeKind,
	load func([]graph.NodeKind) []*graph.Node,
) []*graph.Node {
	if c == nil {
		return load(kinds)
	}
	normalized, key := frameworkNodeKindsCacheKey(kinds)
	if len(normalized) == 0 {
		return nil
	}
	if nodes, ok := c.nodeQueries[key]; ok {
		c.nodeHits++
		return nodes
	}
	c.nodeMisses++
	nodes := load(normalized)
	if !c.retainNodeQuery(key, nodes) {
		c.bypasses++
		return nodes
	}
	return c.nodeQueries[key]
}

func (c *frameworkFullReadCache) retainNodeQuery(key string, nodes []*graph.Node) bool {
	if c == nil {
		return false
	}
	newNodes := make(map[string]*graph.Node)
	newBytes := len(nodes) * 8
	for _, node := range nodes {
		if node == nil || node.ID == "" {
			continue
		}
		if _, exists := c.nodesByID[node.ID]; exists {
			continue
		}
		if _, exists := newNodes[node.ID]; exists {
			continue
		}
		newNodes[node.ID] = node
		newBytes += frameworkNodeBytes(node)
	}
	if !c.canRetain(len(newNodes), newBytes) {
		return false
	}
	for id, node := range newNodes {
		c.nodesByID[id] = node
		c.nodeRows++
		c.nodeBytes += frameworkNodeBytes(node)
	}
	cached := make([]*graph.Node, len(nodes))
	for i, node := range nodes {
		if node != nil && node.ID != "" {
			if canonical := c.nodesByID[node.ID]; canonical != nil {
				cached[i] = canonical
				continue
			}
		}
		cached[i] = node
	}
	c.nodeQueries[key] = cached
	c.queryBytes += len(cached) * 8
	return true
}

func (c *frameworkFullReadCache) memberMethodsByType(
	store graph.Store,
) (map[string][]graph.MemberMethodInfo, bool) {
	reader, ok := store.(graph.MemberMethodsByType)
	if !ok {
		return nil, false
	}
	if c == nil {
		return reader.MemberMethodsByType(), true
	}
	if c.memberMethodsReady {
		c.memberHits++
		return c.memberMethods, true
	}
	c.memberMisses++
	methods := reader.MemberMethodsByType()
	rows, bytes := frameworkMemberMethodsSize(methods)
	if !c.canRetain(rows, bytes) {
		c.bypasses++
		return methods, true
	}
	c.memberMethods = methods
	c.memberMethodsReady = true
	c.memberRows = rows
	c.memberBytes = bytes
	return methods, true
}

func (c *frameworkFullReadCache) canRetain(rows, bytes int) bool {
	if c == nil || c.rowCap <= 0 || c.byteCap <= 0 {
		return false
	}
	return c.nodeRows+c.memberRows+rows <= c.rowCap &&
		c.nodeBytes+c.queryBytes+c.memberBytes+bytes <= c.byteCap
}

func (c *frameworkFullReadCache) invalidateNodes() {
	if c == nil {
		return
	}
	c.nodeQueries = make(map[string][]*graph.Node)
	c.nodesByID = make(map[string]*graph.Node)
	c.nodeRows = 0
	c.nodeBytes = 0
	c.queryBytes = 0
	// MemberMethodInfo includes node name/location data, so any declaration
	// mutation also invalidates the join projection.
	c.invalidateMemberMethods()
}

func (c *frameworkFullReadCache) invalidateMemberMethods() {
	if c == nil {
		return
	}
	c.memberMethods = nil
	c.memberMethodsReady = false
	c.memberRows = 0
	c.memberBytes = 0
}

func (c *frameworkFullReadCache) stats() FrameworkFullReadCacheStats {
	if c == nil {
		return FrameworkFullReadCacheStats{}
	}
	return FrameworkFullReadCacheStats{
		NodeHits:      c.nodeHits,
		NodeMisses:    c.nodeMisses,
		MemberHits:    c.memberHits,
		MemberMisses:  c.memberMisses,
		Bypasses:      c.bypasses,
		RetainedRows:  c.nodeRows + c.memberRows,
		RetainedBytes: c.nodeBytes + c.queryBytes + c.memberBytes,
		RowCap:        c.rowCap,
		ByteCap:       c.byteCap,
	}
}

func frameworkNodeKindsCacheKey(kinds []graph.NodeKind) ([]graph.NodeKind, string) {
	seen := make(map[graph.NodeKind]struct{}, len(kinds))
	normalized := make([]graph.NodeKind, 0, len(kinds))
	var key strings.Builder
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		normalized = append(normalized, kind)
		value := string(kind)
		key.WriteString(strconv.Itoa(len(value)))
		key.WriteByte(':')
		key.WriteString(value)
		key.WriteByte(';')
	}
	return normalized, key.String()
}

func frameworkMemberMethodsSize(methods map[string][]graph.MemberMethodInfo) (rows, bytes int) {
	for typeID, infos := range methods {
		bytes += len(typeID) + 16
		for _, info := range infos {
			rows++
			bytes += 80 + len(info.MethodID) + len(info.Name) +
				len(info.FilePath) + len(info.RepoPrefix)
		}
	}
	return rows, bytes
}
