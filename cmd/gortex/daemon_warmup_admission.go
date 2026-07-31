package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	defaultWarmupMemoryBudgetBytes int64 = 512 << 20
	minWarmupMemoryBudgetBytes     int64 = 32 << 20
	warmupMemoryBudgetEnv                = "GORTEX_WARMUP_MEMORY_MB"
)

// currentGoMemoryBytes matches the runtime memory-limit definition:
// MemStats.Sys - MemStats.HeapReleased. HeapAlloc alone omits stacks, runtime
// metadata, retained spans, and other managed memory that counts toward
// GOMEMLIMIT and would overstate the headroom available to parsing.
func currentGoMemoryBytes(mem runtime.MemStats) uint64 {
	if mem.HeapReleased >= mem.Sys {
		return 0
	}
	return mem.Sys - mem.HeapReleased
}

func currentWarmupMemoryBudgetBytes() (budget int64, current uint64, overrideErr error) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	current = currentGoMemoryBytes(mem)
	limit := debug.SetMemoryLimit(-1)
	budget, overrideErr = resolvedWarmupMemoryBudgetBytes(
		os.Getenv(warmupMemoryBudgetEnv), limit, current,
	)
	return budget, current, overrideErr
}

func resolvedWarmupMemoryBudgetBytes(rawOverride string, goMemoryLimit int64, currentMemory uint64) (int64, error) {
	budget, overrideErr := warmupMemoryBudgetBytes(rawOverride, goMemoryLimit, currentMemory)
	if overrideErr != nil {
		// Ignore only the malformed override. Retain the automatic
		// GOMEMLIMIT/headroom calculation rather than jumping to 512 MiB.
		budget, _ = warmupMemoryBudgetBytes("", goMemoryLimit, currentMemory)
	}
	return budget, overrideErr
}

// warmupMemoryBudgetBytes resolves the daemon-wide parse bytes-in-flight
// budget. An explicit positive MiB override wins. Otherwise at most half of
// remaining Go-managed-memory headroom is admitted, capped at the existing
// 512 MiB per-Indexer default. A small floor guarantees forward progress when
// the process is already close to a configured limit; the shared gate still
// admits only one floor-sized-or-larger file in that state.
func warmupMemoryBudgetBytes(rawOverride string, goMemoryLimit int64, currentMemory uint64) (int64, error) {
	if rawOverride != "" {
		mb, err := strconv.ParseInt(strings.TrimSpace(rawOverride), 10, 64)
		if err != nil || mb <= 0 || mb > (1<<62)/(1<<20) {
			return 0, fmt.Errorf("%s must be a positive MiB integer", warmupMemoryBudgetEnv)
		}
		return mb << 20, nil
	}

	budget := defaultWarmupMemoryBudgetBytes
	if goMemoryLimit <= 0 || goMemoryLimit >= 1<<60 {
		return budget, nil
	}
	headroom := goMemoryLimit - int64(currentMemory)
	if headroom <= 0 {
		return minWarmupMemoryBudgetBytes, nil
	}
	if headroom/2 < budget {
		budget = headroom / 2
	}
	if budget < minWarmupMemoryBudgetBytes {
		budget = minWarmupMemoryBudgetBytes
	}
	return budget, nil
}
