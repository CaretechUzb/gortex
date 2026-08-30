package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/graph"
)

// constSidecarStore is a backend that can answer the constant sidecar — the
// capability *store_sqlite.Store really has and graph.Store does not declare.
type constSidecarStore struct {
	graph.Store
	vals map[string]string
}

func (c *constSidecarStore) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if v, ok := c.vals[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// The gate must not blind the constant sidecar.
//
// This is the third instance of the frameworkRepoGateStore capability hole,
// after CheckoutGrouped and DanglingEdgeTargetReader — and the first that is a
// wrong ANSWER rather than a slow one. The chain is
//
//	pass -> frameworkEdgeBatchStore -> frameworkRepoGateStore -> backend
//
// and the facade in the middle satisfies graph.ConstantValueReader itself, so
// the pass's own assertion succeeds and nothing looks wrong. The facade then
// asks its embedded store, which is the gate, which does not republish the
// capability — so it takes its "adapter has no constant values" fallback and
// returns an EMPTY map with a NIL error. buildConstDerefMap checks exactly
// `reader != nil && err == nil`, both of which hold, and ingests nothing.
//
// Every Go string-constant dereference in the Temporal pass is lost. Java
// constants survive, because they ride on Meta["value"] rather than the
// sidecar, so the failure is partial and asymmetric — which is what kept it
// invisible.
func TestGateMustNotBlindTheConstantSidecar(t *testing.T) {
	t.Parallel()

	const constID = "odoo/workflow.go::GreetWorkflowName"
	backend := &constSidecarStore{vals: map[string]string{constID: "greet-workflow"}}
	ids := []string{constID}

	// Control: ungated, the facade reaches the sidecar. If this ever fails the
	// test below is proving nothing.
	direct, err := newFrameworkEdgeBatchStore(backend).ConstantValuesByNodeIDs(ids)
	require.NoError(t, err)
	require.Equal(t, "greet-workflow", direct[constID],
		"control: without the gate in the chain the sidecar must answer")

	// The workspace shape that wraps: one repository narrows its allow-list, so
	// the gate wraps every pass that list excludes. A workspace holding sibling
	// checkouts wraps every pass unconditionally, by the same constructor.
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{"odoo": odooOnly()})
	gated := newFrameworkRepoGateStore(backend, gate, "temporal", nil)
	require.IsType(t, &frameworkRepoGateStore{}, gated,
		"the gate must actually be in the chain, or this test asserts nothing")

	through, err := newFrameworkEdgeBatchStore(gated).ConstantValuesByNodeIDs(ids)
	require.NoError(t, err)
	require.Equal(t, "greet-workflow", through[constID],
		"the gate refuses WRITES; it must never hide a read. An empty map with a "+
			"nil error is indistinguishable from 'these constants have no literals'")
}

// A backend that genuinely lacks the sidecar keeps the previous
// empty-but-successful answer rather than gaining an error — the forward above
// must fix the hole without inventing a new failure mode for stores that never
// had the capability.
func TestGateConstantSidecarFallsBackWhenTheBackendHasNone(t *testing.T) {
	t.Parallel()

	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{"odoo": odooOnly()})
	gated := newFrameworkRepoGateStore(&recordingStore{}, gate, "temporal", nil)
	require.IsType(t, &frameworkRepoGateStore{}, gated,
		"without the wrapper this passes vacuously — a bare store would answer empty too")

	reader, ok := gated.(graph.ConstantValueReader)
	require.True(t, ok, "the gate declares the capability unconditionally, as Go requires")

	vals, err := reader.ConstantValuesByNodeIDs([]string{"odoo/x.go::K"})
	require.NoError(t, err)
	require.Empty(t, vals)
}
