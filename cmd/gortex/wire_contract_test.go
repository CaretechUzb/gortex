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

// TestWireContractFingerprint is the schema-stability guard for the
// daemon's snapshot wire format. It fingerprints the exported fields of
// every struct that gets gob-encoded into the snapshot and compares the
// hash against a checked-in golden value. Any change to a field name,
// type, or set-membership shifts the hash.
//
// When this test fails:
//
//  1. Additive change (new field only, existing fields untouched) —
//     update the golden value below. Old snapshots still load because
//     gob decodes unknown fields as zero.
//
//  2. Breaking change (rename, remove, retype an existing field) —
//     bump snapshotSchemaVersion in daemon_snapshot.go AND register a
//     migration in snapshotMigrations, THEN update the golden value.
//     Without a migration, deployed daemons reading an old snapshot
//     will discard the cache on upgrade and pay the full-reindex cost.
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
		{"snapshotHeader", reflect.TypeOf(snapshotHeader{}), ""},
		{"snapshotRepo", reflect.TypeOf(snapshotRepo{}), ""},
		{"snapshotContract", reflect.TypeOf(snapshotContract{}), ""},
	}

	// Golden values computed by fingerprintType against the current
	// struct definitions. When a change to any of these structs is
	// intentional, update this map with the new values (run the test
	// once, copy the "got" hash from the failure message).
	golden := map[string]string{
		"graph.Node":       wireContractGolden("graph.Node"),
		"graph.Edge":       wireContractGolden("graph.Edge"),
		"snapshotHeader":   wireContractGolden("snapshotHeader"),
		"snapshotRepo":     wireContractGolden("snapshotRepo"),
		"snapshotContract": wireContractGolden("snapshotContract"),
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

If the change is ADDITIVE (new field, existing fields untouched):
  • Old snapshots still decode cleanly (gob reads unknown fields as zero).
  • Update the golden fingerprint in wireContractGolden().

If the change RENAMES / REMOVES / RETYPES an existing field:
  • Old snapshots will fail to decode cleanly on the changed field.
  • Bump snapshotSchemaVersion in daemon_snapshot.go.
  • Register a migration in snapshotMigrations for the old→new version.
  • Then update the golden fingerprint.

Field set: %s`, c.name, got, c.want, describeFields(c.typ))
			}
		})
	}
}

// fingerprintType returns a stable SHA-256 over the exported-field set
// of t. Field order is NOT part of the fingerprint (gob identifies
// fields by name), but name + type are. Nested structs are captured by
// their type.String() — if a nested type's own shape changes, callers
// with their own fingerprint will catch it.
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

// wireContractGolden holds the expected fingerprint for each wire
// type. Updated intentionally when a struct changes; see the doc on
// TestWireContractFingerprint for the decision tree (additive update vs
// schema bump + migration). Values are pinned hashes — NOT recomputed
// from the live type — so a field-level drift will surface here.
func wireContractGolden(name string) string {
	switch name {
	case "graph.Node":
		// Bumped when the source column-offset fields StartColumn / EndColumn
		// were added (promoted to typed nodes columns on the SQLite backend).
		// Additive: gob decodes unknown fields as zero, so older snapshots
		// still load with them blank.
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
		// false. Additive: gob decodes older snapshots with it false; not
		// part of the edge identity / dedup key.
		// (Previously bumped when Alias — the renamed name carried by a
		// per-binding import or re-export edge — then Via, ReturnUsage,
		// Context, and Tier, were added.)
		return "d27d737c6c164243e2d55b4ca265a46d658e9617546f20306e9357e17aa46603"
	case "snapshotHeader":
		// Bumped when the VectorIndex / VectorDims / VectorCount fields
		// were added (additive — gob decodes unknown fields as zero).
		// This change shipped alongside snapshotSchemaVersion v3 and a
		// v2→v3 migration so deployed daemons keep their v2 snapshots
		// across the upgrade instead of paying a full re-index.
		// Previously bumped when BinaryMtimeUnix was added.
		return "a0f604616d04c757ffec4d084fc53cf6c3db03c8add1a8af0f32f902c1b1fe92"
	case "snapshotRepo":
		// Bumped when the per-repo NodeCount / EdgeCount fields were added —
		// the baseline the boot shape-degradation guard compares a reloaded
		// repo against. Additive: gob decodes older snapshots with both zero,
		// which the guard reads as "no baseline to compare" and skips.
		return "ddb90a625b70b681e3ac4015db3192991c37e6cdf8b0edef501db282bdfe3de3"
	case "snapshotContract":
		// New wire type introduced with per-repo contract persistence.
		// Mirrors contracts.Contract but stores Type/Role as strings so
		// the snapshot stays decoupled from runtime type aliases.
		return "17b073a2f334b46d5f360a15f3d5d1617bee20660221f1de513735cedd75799c"
	default:
		panic(fmt.Sprintf("no golden fingerprint for %s", name))
	}
}
