package mcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIndexHealthNeedsRefresh(t *testing.T) {
	s := &Server{}

	assert.True(t, s.indexHealthNeedsRefresh(time.Time{}))
	assert.False(t, s.indexHealthNeedsRefresh(time.Now()))
	assert.True(t, s.indexHealthNeedsRefresh(time.Now().Add(-indexHealthSnapshotTTL-time.Second)))
}

func TestCompactIndexHealthUsesCachedPayload(t *testing.T) {
	got := compactIndexHealth(map[string]any{
		"health_score": 99.5,
		"node_count":   42,
		"stale_files":  []string{"a.go"},
		"parse_failures": map[string]string{
			"b.go": "parse error",
		},
	}, time.Now().Add(-2*time.Second), true)

	assert.Contains(t, got, "health=99.5")
	assert.Contains(t, got, "nodes=42")
	assert.Contains(t, got, "stale=1")
	assert.Contains(t, got, "failures=1")
	assert.Contains(t, got, "status=refreshing")
}
