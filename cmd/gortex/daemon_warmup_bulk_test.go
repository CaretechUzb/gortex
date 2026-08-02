package main

import (
	"errors"
	"testing"
)

func TestDrainGoModAndFinalizeWarmupBulkOrdersDrainBeforeFinalize(t *testing.T) {
	var order []string
	wantErr := errors.New("finalize failed")

	_, _, err := drainGoModAndFinalizeWarmupBulk(
		func() { order = append(order, "drain") },
		func() error {
			order = append(order, "finalize")
			return wantErr
		},
	)

	if !errors.Is(err, wantErr) {
		t.Fatalf("finalize error = %v, want %v", err, wantErr)
	}
	if len(order) != 2 || order[0] != "drain" || order[1] != "finalize" {
		t.Fatalf("operation order = %v, want [drain finalize]", order)
	}
}

func TestDrainGoModAndFinalizeWarmupBulkWithoutBulkFinalizer(t *testing.T) {
	drains := 0
	_, finalizeElapsed, err := drainGoModAndFinalizeWarmupBulk(func() { drains++ }, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drains != 1 {
		t.Fatalf("drains = %d, want 1", drains)
	}
	if finalizeElapsed != 0 {
		t.Fatalf("finalize elapsed = %s, want 0", finalizeElapsed)
	}
}
