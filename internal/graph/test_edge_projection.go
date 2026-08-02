package graph

// TestNodeProjection is the metadata-free node census required by the global
// test classification pass. Rows are read-only: callers that need to mutate a
// node must fetch the full node by ID first.
type TestNodeProjection struct {
	ID       string
	Kind     NodeKind
	Name     string
	FilePath string
	Language string
}

// TestEdgeProjection is the metadata-free edge census required by annotation
// classification and EdgeCalls -> EdgeTests synthesis. Kind is retained so a
// single scanner contract can safely serve either requested edge kind.
type TestEdgeProjection struct {
	From     string
	To       string
	Kind     EdgeKind
	FilePath string
	Line     int
}

// TestProjectionScanner is an optional fast path for the whole-graph test pass.
// Implementations must close each backing cursor before invoking yield and use
// a stable high-water mark so callbacks may safely re-enter the store. Pages
// must contain at most pageSize rows; returning false stops the scan.
type TestProjectionScanner interface {
	ScanTestNodeProjections(kinds []NodeKind, pageSize int, yield func([]TestNodeProjection) bool)
	ScanTestEdgeProjections(kinds []EdgeKind, pageSize int, yield func([]TestEdgeProjection) bool)
}
