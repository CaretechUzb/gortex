package graph

import (
	"iter"
	"sort"
)

// OverlayLayerReader is the read surface OverlaidView composes over a
// base Reader. It answers three questions about one immutable layer:
// which files the layer covers, which nodes and edges it carries, and
// which base identities it hides. Nothing else about a layer is
// consulted by the composition, so any type that answers these composes
// with a base graph exactly like the in-memory *OverlayLayer does.
//
// Implementations are read-only once handed to a view: the composition
// never writes back, calls concurrently from one request, and treats
// every returned slice as owned by the layer. Iteration order is
// unspecified except where a method documents one.
type OverlayLayerReader interface {
	// HasFile reports whether the layer covers a graph path — either
	// with replacement content or as a tombstone. A covered path is
	// answered from the layer alone; base's view of it stays hidden.
	HasFile(graphPath string) bool

	// IsTombstone reports whether a covered path carries no content at
	// all, so every base node and edge anchored in it disappears.
	IsTombstone(graphPath string) bool

	// FilePaths lists every covered path in sorted order.
	FilePaths() []string

	// CoversNodeID reports whether the layer covers the file a node ID
	// belongs to. A symbol ID carries its file before the "::"
	// separator; a file node's ID is the bare path with no separator.
	CoversNodeID(id string) bool

	// OwnsNodeIdentity reports whether the layer speaks for an ID
	// itself — it carries a node under that ID or marked the ID
	// removed — which holds for identities whose file it does not
	// cover.
	OwnsNodeIdentity(id string) bool

	// OwnsOutEdges reports whether the layer speaks for a node's whole
	// outgoing edge set, so base's edges out of it are hidden and the
	// layer's are the only ones the view exposes.
	//
	// It is a wider claim than OwnsNodeIdentity and a separate one: a
	// layer may replace what a node points at while the node itself
	// keeps living in the layer below, which is what happens when a
	// rename in one file retargets the calls made from an untouched
	// one. Answering yes here says nothing about whether the layer
	// carries the node — that stays OwnsNodeIdentity's question.
	OwnsOutEdges(id string) bool

	// IsRemovedID reports whether the layer marked a base ID removed.
	IsRemovedID(id string) bool

	// RemovedIDs iterates every base ID the layer marked removed.
	RemovedIDs() iter.Seq[string]

	// IsNameRemoved reports whether the layer hid the base ID that base
	// carries under this short name.
	IsNameRemoved(name, id string) bool

	// RemovedIDsForName lists, in sorted order, the base IDs hidden
	// under one short name.
	RemovedIDsForName(name string) []string

	// NodeByID returns the layer's node for an ID, or nil when it
	// carries none — including when it owns the identity and hid it.
	NodeByID(id string) *Node

	// NodeByQualName returns the layer's node for a qualified name.
	NodeByQualName(qualName string) *Node

	// NodesByName returns the layer's nodes carrying one short name.
	NodesByName(name string) []*Node

	// NamedNodes iterates the layer's short names with the nodes
	// carrying each. Names the layer holds no node for are absent.
	NamedNodes() iter.Seq2[string, []*Node]

	// Nodes iterates every node the layer carries.
	Nodes() iter.Seq[*Node]

	// FileNodes returns the layer's nodes for a covered path — nil for
	// a tombstone and for an uncovered path.
	FileNodes(graphPath string) []*Node

	// OutEdges returns the layer's resolved edges leaving one node.
	OutEdges(nodeID string) []*Edge

	// InEdges returns the layer's resolved edges entering one node.
	InEdges(nodeID string) []*Edge

	// Edges iterates every edge the layer introduces.
	Edges() iter.Seq[*Edge]
}

// *OverlayLayer is the in-memory implementation of the contract; the
// methods below are its answers to the questions above.
var _ OverlayLayerReader = (*OverlayLayer)(nil)

// CoversNodeID reports whether the layer covers the file an ID belongs
// to. A symbol ID carries its file before the `::` separator, so IDFile
// answers for it. A file node's ID is the bare path with no separator,
// so IDFile returns "" and the file the ID belongs to is the ID itself —
// checked against the layer's covered-path set, never guessed. IDs of
// other bare shapes are not paths, so they simply miss that set.
func (l *OverlayLayer) CoversNodeID(id string) bool {
	if l == nil || id == "" {
		return false
	}
	if file := IDFile(id); file != "" {
		return l.HasFile(file)
	}
	return l.HasFile(id)
}

// OwnsNodeIdentity reports whether the layer speaks for an ID whether or
// not it covers the ID's file: a re-emitted node and a removal marker
// both make the layer the authority for that identity.
func (l *OverlayLayer) OwnsNodeIdentity(id string) bool {
	if l == nil || id == "" {
		return false
	}
	return l.nodeByID[id] != nil || l.removedByID[id]
}

// OwnsOutEdges reports whether the layer speaks for a node's outgoing
// edge set. An in-memory layer parses whole buffers, so it owns exactly
// the adjacency of the identities it owns: a covered file's symbols and
// the identities it marked removed. It never replaces the adjacency of a
// node it does not otherwise speak for.
func (l *OverlayLayer) OwnsOutEdges(id string) bool {
	return l.CoversNodeID(id) || l.OwnsNodeIdentity(id)
}

// IsRemovedID reports whether MarkRemoved recorded this base ID.
func (l *OverlayLayer) IsRemovedID(id string) bool {
	if l == nil {
		return false
	}
	return l.removedByID[id]
}

// RemovedIDs iterates the removal index by identity.
func (l *OverlayLayer) RemovedIDs() iter.Seq[string] {
	return func(yield func(string) bool) {
		if l == nil {
			return
		}
		for id := range l.removedByID {
			if !yield(id) {
				return
			}
		}
	}
}

// IsNameRemoved reports whether the layer hid a base ID that base
// carries under this short name.
func (l *OverlayLayer) IsNameRemoved(name, id string) bool {
	if l == nil {
		return false
	}
	removed := l.nameRemoved[name]
	return removed != nil && removed[id]
}

// RemovedIDsForName lists the base IDs hidden under one short name.
func (l *OverlayLayer) RemovedIDsForName(name string) []string {
	if l == nil {
		return nil
	}
	removed := l.nameRemoved[name]
	if len(removed) == 0 {
		return nil
	}
	out := make([]string, 0, len(removed))
	for id := range removed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// NodeByID returns the layer's node for an ID. A nil result for an ID
// the layer owns means the overlay dropped the symbol.
func (l *OverlayLayer) NodeByID(id string) *Node {
	if l == nil {
		return nil
	}
	return l.nodeByID[id]
}

// NodeByQualName returns the layer's node for a qualified name.
func (l *OverlayLayer) NodeByQualName(qualName string) *Node {
	if l == nil {
		return nil
	}
	return l.nodesByQual[qualName]
}

// NamedNodes iterates the layer's short-name index.
func (l *OverlayLayer) NamedNodes() iter.Seq2[string, []*Node] {
	return func(yield func(string, []*Node) bool) {
		if l == nil {
			return
		}
		for name, bucket := range l.nodesByName {
			if !yield(name, bucket) {
				return
			}
		}
	}
}

// Nodes iterates every node the layer carries.
func (l *OverlayLayer) Nodes() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if l == nil {
			return
		}
		for _, n := range l.nodeByID {
			if !yield(n) {
				return
			}
		}
	}
}

// FileNodes returns the layer's own slice of nodes for a covered path.
// Construction is complete before a view is published, so a reader can
// scan it without a snapshot copy — and must not write to it.
func (l *OverlayLayer) FileNodes(graphPath string) []*Node {
	if l == nil {
		return nil
	}
	entry := l.entries[graphPath]
	if entry == nil || entry.Deleted {
		return nil
	}
	return entry.Nodes
}

// OutEdges returns the layer's resolved edges leaving one node.
func (l *OverlayLayer) OutEdges(nodeID string) []*Edge {
	if l == nil {
		return nil
	}
	return l.outEdges[nodeID]
}

// InEdges returns the layer's resolved edges entering one node.
func (l *OverlayLayer) InEdges(nodeID string) []*Edge {
	if l == nil {
		return nil
	}
	return l.inEdges[nodeID]
}

// Edges iterates every edge the layer introduces, source by source.
func (l *OverlayLayer) Edges() iter.Seq[*Edge] {
	return func(yield func(*Edge) bool) {
		if l == nil {
			return
		}
		for _, edges := range l.outEdges {
			for _, e := range edges {
				if !yield(e) {
					return
				}
			}
		}
	}
}
