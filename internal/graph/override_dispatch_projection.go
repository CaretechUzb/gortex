package graph

// DefaultOverrideDispatchCallPageSize bounds compact override-dispatch pages.
// Backends close their read cursor before yielding a page so callbacks may
// safely re-enter the store for exact identity lookup.
const DefaultOverrideDispatchCallPageSize = 256

// OverrideDispatchCall is the compact admission row shared by the Java and PHP
// override passes. The edge identity is exact; caller language is read from the
// source node without materializing that node.
type OverrideDispatchCall struct {
	EdgeIdentity
	CallerLanguage string
}

// OverrideDispatchCallBatchScanner is an optional compact backend capability.
// A nil repository scope means all repositories; a non-nil empty scope means
// no rows. Implementations must freeze a high-water mark and close each cursor
// before invoking yield.
type OverrideDispatchCallBatchScanner interface {
	ScanOverrideDispatchCalls(
		repoPrefixes []string,
		pageSize int,
		yield func([]OverrideDispatchCall) bool,
	)
}

// ScanOverrideDispatchCalls selects the compact backend capability when
// available. The fallback preserves ordinary Store semantics and is primarily
// for the in-memory graph and adapters; production SQLite implements the
// projection without decoding full edges or caller nodes.
func ScanOverrideDispatchCalls(
	s Store,
	repoPrefixes []string,
	pageSize int,
	yield func([]OverrideDispatchCall) bool,
) {
	if s == nil || yield == nil || (repoPrefixes != nil && len(repoPrefixes) == 0) {
		return
	}
	if scanner, ok := s.(OverrideDispatchCallBatchScanner); ok {
		scanner.ScanOverrideDispatchCalls(repoPrefixes, pageSize, yield)
		return
	}
	if pageSize <= 0 {
		pageSize = DefaultOverrideDispatchCallPageSize
	}

	var scope map[string]struct{}
	if repoPrefixes != nil {
		scope = make(map[string]struct{}, len(repoPrefixes))
		for _, prefix := range repoPrefixes {
			scope[prefix] = struct{}{}
		}
		if len(scope) == 0 {
			return
		}
	}

	page := make([]OverrideDispatchCall, 0, pageSize)
	flush := func() bool {
		if len(page) == 0 {
			return true
		}
		current := page
		page = make([]OverrideDispatchCall, 0, pageSize)
		return yield(current)
	}
	for edge := range s.EdgesByKind(EdgeCalls) {
		if edge == nil || !IsUnresolvedTarget(edge.To) || edge.IsSpeculative() {
			continue
		}
		if scope != nil {
			if _, ok := scope[RepoPrefixOfID(edge.From)]; !ok {
				continue
			}
		}
		caller := s.GetNode(edge.From)
		if caller == nil || (caller.Language != "java" && caller.Language != "php") {
			continue
		}
		page = append(page, OverrideDispatchCall{
			EdgeIdentity: EdgeIdentityFor(edge), CallerLanguage: caller.Language,
		})
		if len(page) == pageSize && !flush() {
			return
		}
	}
	flush()
}
