package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBatchScopeAndCensusBelongOnlyToEndBatch(t *testing.T) {
	mi := &MultiIndexer{}

	// Arming outside an active batch is inert.
	mi.ArmBatchScope(map[string]struct{}{"ignored": {}})
	mi.ArmBatchCensusEligible()
	assert.Nil(t, mi.batchChangedPrefixes)
	assert.False(t, mi.batchCensusEligible)

	mi.BeginBatch()
	first := map[string]struct{}{"repo-a": {}}
	mi.ArmBatchScope(first)
	delete(first, "repo-a")
	first["caller-mutation"] = struct{}{}
	mi.ArmBatchScope(map[string]struct{}{"repo-b": {}})
	mi.ArmBatchCensusEligible()

	mi.EndBatch()
	mi.mu.RLock()
	assert.Nil(t, mi.batchChangedPrefixes)
	assert.False(t, mi.batchCensusEligible)
	assert.False(t, mi.deferGlobalPasses)
	assert.False(t, mi.deferResolve)
	mi.mu.RUnlock()

	var nilIndexer *MultiIndexer
	nilIndexer.ArmBatchCensusEligible()
}
