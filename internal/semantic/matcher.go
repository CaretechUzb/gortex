package semantic

// SymbolMap provides bidirectional mapping between external symbol identifiers
// (SCIP URIs, go/types object IDs, LSP URIs) and Gortex node IDs.
type SymbolMap struct {
	externalToGortex map[string]string
	gortexToExternal map[string]string
}

// NewSymbolMap creates an empty symbol map.
func NewSymbolMap() *SymbolMap {
	return &SymbolMap{
		externalToGortex: make(map[string]string),
		gortexToExternal: make(map[string]string),
	}
}

// Add registers a mapping between an external symbol ID and a Gortex node ID.
func (m *SymbolMap) Add(externalID, gortexID string) {
	m.externalToGortex[externalID] = gortexID
	m.gortexToExternal[gortexID] = externalID
}

// GortexID looks up the Gortex node ID for an external symbol.
func (m *SymbolMap) GortexID(externalID string) (string, bool) {
	id, ok := m.externalToGortex[externalID]
	return id, ok
}
