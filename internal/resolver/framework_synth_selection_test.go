package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestFrameworkSynthesizerSelectionRejectsUnknownAndDuplicateNames(t *testing.T) {
	_, err := NewFrameworkSynthesizerSelection([]string{"not-a-synthesizer"})
	assert.ErrorContains(t, err, `unknown framework synthesizer "not-a-synthesizer"`)

	name := defaultFrameworkSynthesizers()[0].Name()
	_, err = NewFrameworkSynthesizerSelection([]string{name, name})
	assert.ErrorContains(t, err, `duplicate framework synthesizer "`+name+`"`)
}

func TestFrameworkSynthesizerSelectionCatalogIsUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for _, name := range FrameworkSynthesizerNames() {
		if _, exists := seen[name]; exists {
			t.Fatalf("duplicate framework registry name %q", name)
		}
		seen[name] = struct{}{}
	}
}

func TestFrameworkSynthesizerSelectionExplicitEmptySkipsEveryGraphScan(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{})
	require.NoError(t, err)
	store := &countingFrameworkLightStore{Store: graph.New()}

	report := RunFrameworkSynthesizersWithSelection(store, selection)

	assert.Zero(t, report.Total)
	assert.Empty(t, report.Per)
	assert.Zero(t, store.nodeCalls)
	assert.Zero(t, store.edgeCalls)
}

func TestFrameworkSynthesizerSelectionRunsOnlySelectedSynthesizer(t *testing.T) {
	name := defaultFrameworkSynthesizers()[0].Name()
	selection, err := NewFrameworkSynthesizerSelection([]string{name})
	require.NoError(t, err)

	report := RunFrameworkSynthesizersWithSelection(graph.New(), selection)

	require.Len(t, report.Per, 1)
	assert.Equal(t, name, report.Per[0].Name)
}

func TestFrameworkSynthesizerSelectionRunsOnlySelectedClaimingResolver(t *testing.T) {
	require.NotEmpty(t, defaultClaimingResolvers())
	name := defaultClaimingResolvers()[0].Name()
	selection, err := NewFrameworkSynthesizerSelection([]string{name})
	require.NoError(t, err)

	report := RunFrameworkSynthesizersWithSelection(graph.New(), selection)

	require.Len(t, report.Per, 1)
	assert.Equal(t, name, report.Per[0].Name)
}
