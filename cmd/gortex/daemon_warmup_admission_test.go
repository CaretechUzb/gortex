package main

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrentGoMemoryBytesMatchesRuntimeLimitMetric(t *testing.T) {
	tests := []struct {
		name string
		mem  runtime.MemStats
		want uint64
	}{
		{name: "subtracts released pages", mem: runtime.MemStats{Sys: 1_000, HeapReleased: 300}, want: 700},
		{name: "equal is zero", mem: runtime.MemStats{Sys: 1_000, HeapReleased: 1_000}, want: 0},
		{name: "greater is guarded", mem: runtime.MemStats{Sys: 1_000, HeapReleased: 1_100}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, currentGoMemoryBytes(tt.mem))
		})
	}
}

func TestWarmupMemoryBudgetBytes(t *testing.T) {
	const mib = int64(1 << 20)
	tests := []struct {
		name     string
		override string
		limit    int64
		current  uint64
		want     int64
		wantErr  bool
	}{
		{name: "explicit override", override: "768", limit: 256 * mib, current: 240 << 20, want: 768 * mib},
		{name: "unlimited uses default", limit: 1 << 62, want: defaultWarmupMemoryBudgetBytes},
		{name: "half remaining headroom", limit: 1 << 30, current: 256 << 20, want: 384 * mib},
		{name: "headroom is capped", limit: 4 << 30, current: 128 << 20, want: defaultWarmupMemoryBudgetBytes},
		{name: "near limit keeps progress floor", limit: 256 * mib, current: 224 << 20, want: minWarmupMemoryBudgetBytes},
		{name: "at limit keeps progress floor", limit: 256 * mib, current: 256 << 20, want: minWarmupMemoryBudgetBytes},
		{name: "invalid override", override: "many", limit: 1 << 62, wantErr: true},
		{name: "zero override", override: "0", limit: 1 << 62, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := warmupMemoryBudgetBytes(tt.override, tt.limit, tt.current)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolvedWarmupMemoryBudgetInvalidOverrideKeepsAutomaticHeadroom(t *testing.T) {
	const mib = int64(1 << 20)
	budget, err := resolvedWarmupMemoryBudgetBytes("many", 256*mib, 224<<20)
	require.Error(t, err)
	assert.Equal(t, minWarmupMemoryBudgetBytes, budget)
}
