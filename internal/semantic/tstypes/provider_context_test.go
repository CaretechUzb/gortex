package tstypes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic"
)

func TestEnrichRepoContextCancelledBeforeApplyIsMutationFree(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"a/Svc.java": javaSvc,
		"b/App.java": `package b;
import a.Svc;
public class App { public void main() { new Svc().run(); } }
`,
	})
	p := NewProvider(JavaSpec(), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := p.EnrichRepoContext(ctx, g, "", dir, func(int) time.Duration { return 0 })
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Partial)
	assert.Equal(t, semantic.EnrichBoundBudget, result.BoundReason)
	assert.Zero(t, result.EdgesAdded)
	assert.Zero(t, result.EdgesConfirmed)
	// A pass cancelled before runPass's first ctx.Err() check still records
	// the file count it was about to stage (len(files) is known before the
	// cancellation check), but never enters staging or apply: StagingMs,
	// ApplyMs, Phase and PagesApplied must all stay at their zero value so a
	// Partial read from this branch cannot be misread as "cut mid-apply".
	assert.Equal(t, 2, result.FileCount)
	assert.Zero(t, result.StagingMs)
	assert.Zero(t, result.ApplyMs)
	assert.Empty(t, result.Phase)
	assert.Zero(t, result.PagesApplied)
}
