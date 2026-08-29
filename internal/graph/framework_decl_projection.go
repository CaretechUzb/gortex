package graph

import "iter"

// Framework declaration indexes are the maps a dispatch synthesizer builds
// before it can bind anything: an Odoo `_name` to the classes declaring it,
// an external ID to its record, an OWL template to its <t t-name>. Every one
// of them is keyed off node metadata, so Meta has to cross the storage
// boundary — but nothing else heavy does. Doc comments, signatures, section
// text and the four search projections stay behind it.
//
// The distinction matters because these indexes are whole-store by
// construction. A reference in one repository binds to a declaration in
// another, so no repository scope narrows the build; the only lever is how
// much of each row is decoded. Reading complete nodes instead of this
// projection was measured at ~253s per Odoo synthesis pass on a 855k-node
// workspace, against a store where the declarations themselves number a few
// hundred thousand.

// FrameworkDeclNode is the exact node row a framework declaration index
// retains.
type FrameworkDeclNode struct {
	ID       string
	Kind     NodeKind
	Name     string
	FilePath string
	Language string
	Meta     map[string]any
}

// FrameworkDeclNodeSequencer streams declaration rows for an exact kind set.
type FrameworkDeclNodeSequencer interface {
	FrameworkDeclNodesSeq(kinds ...NodeKind) iter.Seq[FrameworkDeclNode]
}

// FrameworkDeclNodesSeq selects the compact capability when available. The
// compatibility fallback preserves third-party Store behavior.
//
// Rows arrive grouped by kind, in the order the backend enumerates them —
// callers that need one kind's index complete before another's walk starts
// must issue two calls rather than rely on the grouping.
func FrameworkDeclNodesSeq(s Store, kinds ...NodeKind) iter.Seq[FrameworkDeclNode] {
	if s == nil || len(kinds) == 0 {
		return func(func(FrameworkDeclNode) bool) {}
	}
	if seq, ok := s.(FrameworkDeclNodeSequencer); ok {
		return seq.FrameworkDeclNodesSeq(kinds...)
	}
	return func(yield func(FrameworkDeclNode) bool) {
		for node := range NodesByKindsSeq(s, kinds...) {
			if node == nil {
				continue
			}
			if !yield(FrameworkDeclNode{
				ID:       node.ID,
				Kind:     node.Kind,
				Name:     node.Name,
				FilePath: node.FilePath,
				Language: node.Language,
				Meta:     node.Meta,
			}) {
				return
			}
		}
	}
}

// FrameworkDeclIdentity is the row for the kinds a declaration index joins
// by node ID alone — methods, fields, addon modules. None of them carries
// the declaration; they inherit it from the owner their ID names, so no
// metadata has to cross the boundary for them at all.
type FrameworkDeclIdentity struct {
	ID   string
	Kind NodeKind
	Name string
}

// FrameworkDeclIdentitySequencer streams identity rows for an exact kind set.
type FrameworkDeclIdentitySequencer interface {
	FrameworkDeclIdentitiesSeq(kinds ...NodeKind) iter.Seq[FrameworkDeclIdentity]
}

// FrameworkDeclIdentitiesSeq selects the compact capability when available.
//
// NodeIDNamesByKindsSeq projects the same two useful columns and would have
// served, except that it orders by name — a filesort over the whole method
// table, for a consumer that builds an unordered map. This one inherits the
// paged projection's id order, which is the primary key.
func FrameworkDeclIdentitiesSeq(s Store, kinds ...NodeKind) iter.Seq[FrameworkDeclIdentity] {
	if s == nil || len(kinds) == 0 {
		return func(func(FrameworkDeclIdentity) bool) {}
	}
	if seq, ok := s.(FrameworkDeclIdentitySequencer); ok {
		return seq.FrameworkDeclIdentitiesSeq(kinds...)
	}
	return func(yield func(FrameworkDeclIdentity) bool) {
		for node := range NodesByKindsSeq(s, kinds...) {
			if node == nil {
				continue
			}
			if !yield(FrameworkDeclIdentity{
				ID: node.ID, Kind: node.Kind, Name: node.Name,
			}) {
				return
			}
		}
	}
}
