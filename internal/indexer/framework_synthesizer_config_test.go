package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/config"
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
