package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestWireContractFingerprint is the schema-stability guard for the two
// types the store persists row by row. It fingerprints the exported
// fields of graph.Node and graph.Edge and compares the hash against a
// checked-in golden value; any change to a field name, type, or
// set-membership shifts the hash.
//
// The point is that a new field is not free: the backend has to learn to
// write and read it, or the value silently vanishes on the next restart.
// When this test fails, either teach the store the new field or
// deliberately accept that it is in-memory-only — then update the golden
// value below with a note saying which.
//
// Runs as part of the existing `go test ./...` sweep, no extra CI
// infrastructure required.
func TestWireContractFingerprint(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"graph.Node", reflect.TypeOf(graph.Node{}), ""},
		{"graph.Edge", reflect.TypeOf(graph.Edge{}), ""},
	}

	// Golden values computed by fingerprintType against the current
	// struct definitions. When a change to any of these structs is
	// intentional, update this map with the new values (run the test
	// once, copy the "got" hash from the failure message).
	golden := map[string]string{
		"graph.Node": wireContractGolden("graph.Node"),
		"graph.Edge": wireContractGolden("graph.Edge"),
	}

	for i := range cases {
		cases[i].want = golden[cases[i].name]
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fingerprintType(c.typ)
			if got != c.want {
				t.Errorf(`wire contract for %s changed.
  got:  %s
  want: %s

Check that the store persists the new / changed field — a field the
backend never writes reads back zero after a restart. Then update the
golden fingerprint in wireContractGolden(), noting whether the field is
persisted or deliberately in-memory-only.

Field set: %s`, c.name, got, c.want, describeFields(c.typ))
			}
		})
	}
}

// fingerprintType returns a stable SHA-256 over the exported-field set
// of t. Field order is NOT part of the fingerprint, but name + type are.
// Nested structs are captured by their type.String() — if a nested
// type's own shape changes, callers with their own fingerprint will
// catch it.
func fingerprintType(t reflect.Type) string {
	if t.Kind() != reflect.Struct {
		return ""
	}
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	sort.Strings(fields)
	h := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(h[:])
}

// describeFields renders the exported-field set for failure messages.
// Matches fingerprintType's canonical form so the output is a drop-in
// replacement for the hash during debugging.
func describeFields(t reflect.Type) string {
	if t.Kind() != reflect.Struct {
		return "(not a struct)"
	}
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	sort.Strings(fields)
	return strings.Join(fields, ", ")
}

// wireContractGolden holds the expected fingerprint for each persisted
// type. Updated intentionally when a struct changes; see the doc on
// TestWireContractFingerprint. Values are pinned hashes — NOT recomputed
// from the live type — so a field-level drift will surface here.
func wireContractGolden(name string) string {
	switch name {
	case "graph.Node":
		// Bumped when the source column-offset fields StartColumn / EndColumn
		// were added (promoted to typed nodes columns on the SQLite backend).
		// Previously bumped when the federation proxy-node fields Origin /
		// Stub / FetchedAt, then AbsoluteFilePath, then WorkspaceID /
		// ProjectID, were added.
		return "a600bd906c9e09ffa0828427f5d6622acd5fe244798a61d5712c151053be5dac"
	case "graph.Edge":
		// Bumped when NameOnly was added — the marker on a synthesized
		// name-only candidate row (a call site that named the symbol but
		// never bound to it, projected onto it by find_usages /
		// get_callers under min_tier:"text_matched"). Set only on the
		// in-memory projection, so a persisted edge always carries it
		// false; not part of the edge identity / dedup key.
		// (Previously bumped when Alias — the renamed name carried by a
		// per-binding import or re-export edge — then Via, ReturnUsage,
		// Context, and Tier, were added.)
		return "d27d737c6c164243e2d55b4ca265a46d658e9617546f20306e9357e17aa46603"
	default:
		panic(fmt.Sprintf("no golden fingerprint for %s", name))
	}
}
