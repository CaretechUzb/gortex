package goanalysis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/semantic"
)

func TestLargeGoTypesAdmissionClassification(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		fullRepo bool
		want     bool
	}{
		{name: "unknown full repo is conservative", ctx: context.Background(), fullRepo: true, want: true},
		{name: "small full repo remains shareable", ctx: semantic.WithEnrichmentAdmissionNodes(context.Background(), goTypesLargeNodeThreshold-1), fullRepo: true, want: false},
		{name: "threshold full repo is exclusive", ctx: semantic.WithEnrichmentAdmissionNodes(context.Background(), goTypesLargeNodeThreshold), fullRepo: true, want: true},
		{name: "incremental load remains shareable", ctx: semantic.WithEnrichmentAdmissionNodes(context.Background(), goTypesLargeNodeThreshold*2), fullRepo: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := largeGoTypesAdmission(tt.ctx, tt.fullRepo); got != tt.want {
				t.Fatalf("largeGoTypesAdmission() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHeavyAdmissionSerializesLargeAndPreservesLightLane(t *testing.T) {
	provider := &Provider{
		heavyGate: make(chan struct{}, 2),
		largeGate: make(chan struct{}, 1),
	}

	releaseLarge, err := provider.acquireHeavy(context.Background(), true)
	if err != nil {
		t.Fatalf("acquire first large: %v", err)
	}
	releaseLight, err := provider.acquireHeavy(context.Background(), false)
	if err != nil {
		releaseLarge()
		t.Fatalf("acquire light beside large: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := provider.acquireHeavy(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		if err == nil {
			release()
		}
		releaseLight()
		releaseLarge()
		t.Fatalf("second large error = %v, want deadline exceeded", err)
	}

	releaseLight()
	releaseLarge()

	// Once the large lease is gone, both ordinary lanes remain usable; a
	// cancelled large waiter must not leak either permit.
	releaseOne, err := provider.acquireHeavy(context.Background(), false)
	if err != nil {
		t.Fatalf("acquire first light after release: %v", err)
	}
	releaseTwo, err := provider.acquireHeavy(context.Background(), false)
	if err != nil {
		releaseOne()
		t.Fatalf("acquire second light after release: %v", err)
	}
	releaseTwo()
	releaseOne()
}
