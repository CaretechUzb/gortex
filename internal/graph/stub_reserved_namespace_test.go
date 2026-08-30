package graph

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The defect this pins, end to end.
//
// The external-call synthesizer mints `external-call::stdlib::<path>`. StubKind
// checks the bare kinds first, and misses — its constant for that namespace is
// spelled with an underscore while the synthesizer uses a hyphen. So the id
// falls through to the `<repo>::<kind>::…` branch, matches on the `stdlib::`
// second segment, and StubRepoPrefix hands back `external-call` as though it
// were a repository.
//
// Downstream that is not cosmetic. The Go external-call attribution pass keys
// its materialised module nodes on this value, so a live workspace grew
// `external-call::module::go:odoo` carrying RepoPrefix `external-call`; owning
// that node earned the prefix a repo_graph_gen anchor row, and every
// store-wide mutation advanced it from then on.
func TestASyntheticNamespaceIsNotAReadableRepoPrefix(t *testing.T) {
	t.Parallel()
	require.Equal(t, "", StubRepoPrefix("external-call::stdlib::odoo"),
		"the id is nested inside a placeholder namespace; no repository owns it")
	require.Equal(t, "", StubRepoPrefix("external-call::module::go:odoo"))

	// The kind is still read correctly — only the ownership claim is refused.
	require.Equal(t, StubKindStdlib, StubKind("external-call::stdlib::odoo"))
	require.Equal(t, "odoo", StubRest("external-call::stdlib::odoo"))
}

// The guard must not cost a real repository its prefix. Repo-prefixed stubs are
// the whole reason StubRepoPrefix exists, and reading one as global would send
// two repos' distinct stdlib symbols to the same node.
func TestARealRepoKeepsItsStubPrefix(t *testing.T) {
	t.Parallel()
	require.Equal(t, "local", StubRepoPrefix("local::stdlib::re::compile"))
	require.Equal(t, "addons", StubRepoPrefix("addons::module::go:datetime"))
	require.Equal(t, "gortex", StubRepoPrefix(StubID("gortex", StubKindBuiltin, "go", "min")))

	// A bare stub has no owner, and a non-stub id is not this function's
	// business at all — both predate the guard and must stay put.
	require.Equal(t, "", StubRepoPrefix("stdlib::fmt::Errorf"))
	require.Equal(t, "", StubRepoPrefix("local/main.go::Run"))
}

// Every reserved segment is rejected, and the exported list is the same set the
// predicate answers from — the store's repair migration encodes that list into
// SQL, so a member the predicate knows and the list omits would leave rows the
// repair silently skips.
func TestTheReservedListAndThePredicateAgree(t *testing.T) {
	t.Parallel()
	list := ReservedIDNamespaces()
	require.NotEmpty(t, list)
	for _, ns := range list {
		require.True(t, IsReservedIDNamespace(ns), ns)
		require.Equal(t, "", StubRepoPrefix(ns+"::stdlib::x"), ns)
	}
	require.False(t, IsReservedIDNamespace("local"))
	require.False(t, IsReservedIDNamespace(""))

	list[0] = "mutated"
	require.False(t, IsReservedIDNamespace("mutated"),
		"the accessor must hand back a copy; the reserved set is not caller-writable")
}
