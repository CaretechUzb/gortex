package graph

const (
	// EdgeMetaResolveTerminal marks an unresolved edge that a full resolver pass
	// proved cannot currently bind to an in-graph definition.
	EdgeMetaResolveTerminal = "resolve_terminal"
	// EdgeMetaResolveTerminalReason records the resolver's classification for a
	// terminal edge.
	EdgeMetaResolveTerminalReason = "resolve_terminal_reason"
)

// EdgeTerminalStampPersister is the narrow persistence capability used by the
// resolver's terminal reconciliation tail. Implementations update only the two
// promoted terminal columns selected by each edge's exact logical identity;
// every other edge attribute and metadata key must remain untouched.
//
// Nil edges are ignored, missing identities are no-ops, and duplicate
// identities use the last input value. Disk implementations must bound both
// statement and transaction size and invalidate persisted analysis only when a
// row actually changes.
type EdgeTerminalStampPersister interface {
	PersistEdgeTerminalStamps(edges []*Edge)
}
