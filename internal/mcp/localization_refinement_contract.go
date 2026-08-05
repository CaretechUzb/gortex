package mcp

// The refinement page is byte-budgeted (see the tight-budget builder test), so
// this stays the happy-path instruction only. The release that unsticks a wrong
// ranking is named on the refusals, which is where a blocked caller reads.
const localizationRefinementRequiredActionFormat = `Call Gortex MCP read(operation:"source", target:{symbol:%q}); the named symbol is recommended; any returned candidate is permitted only when its ID appears in completion.allowed_symbols; do not call a host file-read tool.`
