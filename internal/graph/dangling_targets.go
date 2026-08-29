package graph

import (
	"sort"
	"strings"
)

// Finding edges whose target no longer exists.
//
// A framework recompute is a FULL recompute of the edges it collects: an edge
// whose target has left the graph is reset to its placeholder. That contract is
// what lets a reference to a deleted record un-bind itself, and it is the one
// part of a scoped pass that cannot be derived from the changed files alone.
//
// The reason is that a scoped pass finds its work by asking the graph about
// things that exist. When a file is re-indexed its old nodes are gone, so an
// edge still pointing at one of them answers to no node lookup, appears in no
// file's node list, and is invisible to every frontier built that way. It is
// also precisely the edge that must be revisited. A whole-repository collection
// finds it by brute force; a narrower one has to ask this question directly.
//
// Asking it is cheap because it is an anti-join over one covering index rather
// than a walk of edge payloads: measured on a 1.1M-edge repository, 27,282
// distinct targets reduce to 537 dangling ones in ~0.4s, and no edge row is
// decoded to get there.

// DanglingEdgeTargetReader reports edge target ids under an id prefix that no
// node answers to. kinds is required and narrows the scan to the edge families
// the caller can act on; an empty kinds list is an empty answer, never a
// whole-graph sweep.
//
// Implementations MUST evaluate the prefix as a half-open byte range rather
// than a LIKE/GLOB pattern, so the query rides an index on the target column,
// and MUST NOT decode edge payloads — the answer is a set of ids, and the
// caller fetches only the edges the ids select.
type DanglingEdgeTargetReader interface {
	DanglingEdgeTargets(idPrefixes []string, kinds []EdgeKind) []string
}

// DanglingEdgeTargets selects the backend capability when it exists and
// otherwise falls back to a kind-bucket walk with the same semantics.
//
// The fallback is complete, not approximate: a scoped framework pass that
// silently skipped un-binding on a backend without the capability would leave
// references pointing at deleted declarations, which is the exact failure the
// caller is trying to avoid. It is slower, which is acceptable for the in-memory
// and test stores it serves and is why the SQLite path exists.
func DanglingEdgeTargets(s Store, idPrefixes []string, kinds []EdgeKind) []string {
	prefixes := nonEmptyStrings(idPrefixes)
	if s == nil || len(prefixes) == 0 || len(kinds) == 0 {
		return nil
	}
	if reader, ok := s.(DanglingEdgeTargetReader); ok {
		return reader.DanglingEdgeTargets(prefixes, kinds)
	}

	candidates := map[string]struct{}{}
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		for edge := range s.EdgesByKind(kind) {
			if edge == nil || edge.To == "" {
				continue
			}
			if _, seen := candidates[edge.To]; seen {
				continue
			}
			if hasAnyPrefix(edge.To, prefixes) {
				candidates[edge.To] = struct{}{}
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	live := s.GetNodesByIDs(ids)
	out := ids[:0]
	for _, id := range ids {
		if live[id] == nil {
			out = append(out, id)
		}
	}
	return out
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// PrefixUpperBound returns the exclusive upper bound of the byte range holding
// every string that starts with prefix, and false when no such bound exists
// (an all-0xFF prefix, whose range runs to the end of the key space).
//
// Incrementing the last byte is what makes a prefix scan an index range. The
// obvious `prefix || 0x7F` shortcut is wrong on this corpus specifically: an id
// carries a file path, paths here are routinely non-ASCII, and every UTF-8
// continuation byte is >= 0x80 — so that bound silently drops exactly the ids
// whose next byte is multi-byte.
func PrefixUpperBound(prefix string) (string, bool) {
	raw := []byte(prefix)
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == 0xFF {
			continue
		}
		bound := make([]byte, i+1)
		copy(bound, raw[:i+1])
		bound[i]++
		return string(bound), true
	}
	return "", false
}
