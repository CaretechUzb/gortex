package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrameworkSynthesizers_AbsentPreservesDefaultRegistry(t *testing.T) {
	cfg, err := Load(writeConfig(t, "index:\n  workers: 1\n"))
	require.NoError(t, err)
	assert.Nil(t, cfg.Index.FrameworkSynthesizers)
}

func TestFrameworkSynthesizers_ExplicitEmptyDisablesRegistry(t *testing.T) {
	cfg, err := Load(writeConfig(t, "index:\n  framework_synthesizers: []\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.Index.FrameworkSynthesizers)
	assert.Empty(t, *cfg.Index.FrameworkSynthesizers)
}

func TestFrameworkSynthesizers_ExplicitAllowListRoundTrips(t *testing.T) {
	cfg, err := Load(writeConfig(t, "index:\n  framework_synthesizers:\n    - value-ref\n    - fn-value-callback\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.Index.FrameworkSynthesizers)
	assert.Equal(t, []string{"value-ref", "fn-value-callback"}, *cfg.Index.FrameworkSynthesizers)
}
