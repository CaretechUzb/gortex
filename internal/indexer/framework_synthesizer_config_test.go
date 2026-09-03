package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

func frameworkSynthesizerNamesPtr(names ...string) *[]string {
	copyOfNames := append([]string(nil), names...)
	return &copyOfNames
}

func TestIndexConfigHashDistinguishesOmittedAndExplicitEmptyFrameworkSelection(t *testing.T) {
	omitted := indexConfigHash(config.IndexConfig{})
	disabled := indexConfigHash(config.IndexConfig{
		FrameworkSynthesizers: frameworkSynthesizerNamesPtr(),
	})

	assert.NotEqual(t, omitted, disabled)
}

func TestIndexConfigHashTreatsFrameworkSelectionAsASet(t *testing.T) {
	first := indexConfigHash(config.IndexConfig{
		FrameworkSynthesizers: frameworkSynthesizerNamesPtr("value-ref", "fn-value-callback"),
	})
	second := indexConfigHash(config.IndexConfig{
		FrameworkSynthesizers: frameworkSynthesizerNamesPtr("fn-value-callback", "value-ref", "value-ref"),
	})

	assert.Equal(t, first, second)
}

func TestMultiIndexerFrameworkSelectionDisablesPipelineWhenEveryRepoOptsOut(t *testing.T) {
	mi := &MultiIndexer{indexers: map[string]*Indexer{
		"first":  {config: config.IndexConfig{FrameworkSynthesizers: frameworkSynthesizerNamesPtr()}},
		"second": {config: config.IndexConfig{FrameworkSynthesizers: frameworkSynthesizerNamesPtr()}},
	}}

	report := resolver.RunFrameworkSynthesizersWithSelection(
		graph.New(), mi.frameworkSynthesizerSelection(nil),
	)
	assert.Empty(t, report.Per)
}

func TestMultiIndexerFrameworkSelectionUnionsParticipatingRepoLists(t *testing.T) {
	mi := &MultiIndexer{indexers: map[string]*Indexer{
		"first": {
			config: config.IndexConfig{
				FrameworkSynthesizers: frameworkSynthesizerNamesPtr("value-ref"),
			},
		},
		"second": {
			config: config.IndexConfig{
				FrameworkSynthesizers: frameworkSynthesizerNamesPtr("fn-value-callback"),
			},
		},
	}}

	report := resolver.RunFrameworkSynthesizersWithSelection(
		graph.New(), mi.frameworkSynthesizerSelection(nil),
	)
	require.Len(t, report.Per, 2)
	names := []string{report.Per[0].Name, report.Per[1].Name}
	assert.ElementsMatch(t, []string{"value-ref", "fn-value-callback"}, names)
}

func TestMultiIndexerFrameworkSelectionScopesOmittedConfigDominance(t *testing.T) {
	mi := &MultiIndexer{indexers: map[string]*Indexer{
		"disabled": {
			config: config.IndexConfig{FrameworkSynthesizers: frameworkSynthesizerNamesPtr()},
		},
		"legacy": {config: config.IndexConfig{}},
	}}

	disabledReport := resolver.RunFrameworkSynthesizersWithSelection(
		graph.New(), mi.frameworkSynthesizerSelection(map[string]bool{"disabled": true}),
	)
	assert.Empty(t, disabledReport.Per)

	legacyReport := resolver.RunFrameworkSynthesizersWithSelection(
		graph.New(), mi.frameworkSynthesizerSelection(nil),
	)
	assert.Len(t, legacyReport.Per, len(resolver.FrameworkSynthesizerNames()))
}
