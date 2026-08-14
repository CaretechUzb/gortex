package semantic

// SymbolMap resolves external symbol identifiers (SCIP URIs, go/types object
// IDs, LSP URIs) to Gortex node IDs. Enrichment providers only ever look up
// in that direction, so no reverse index is kept.
type SymbolMap struct {
	externalToGortex map[string]string
}

// NewSymbolMap creates an empty symbol map.
func NewSymbolMap() *SymbolMap {
	return &SymbolMap{
		externalToGortex: make(map[string]string),
	}
}

// Add registers a mapping between an external symbol ID and a Gortex node ID.
func (m *SymbolMap) Add(externalID, gortexID string) {
	m.externalToGortex[externalID] = gortexID
}

// GortexID looks up the Gortex node ID for an external symbol.
func (m *SymbolMap) GortexID(externalID string) (string, bool) {
	id, ok := m.externalToGortex[externalID]
	return id, ok
}
