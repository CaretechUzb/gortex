package goanalysis

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

func TestGoTypesHeavyGateHeldAcrossApplyBarrier(t *testing.T) {
	provider, root, loads := newApplyGateTestProvider(t)
	applyGate := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = semantic.WithApplyGate(ctx, applyGate)

	first := runApplyGateTestEnrichment(provider, ctx, "first", root)
	waitForGoTypesTestLoad(t, loads)
	second := runApplyGateTestEnrichment(provider, ctx, "second", root)

	select {
	case <-loads:
		t.Fatal("second package load started while first enrichment held the heavy gate at the apply barrier")
	case <-time.After(200 * time.Millisecond):
	}

	close(applyGate)
	waitForGoTypesTestLoad(t, loads)
	waitForGoTypesTestResult(t, first, nil)
	waitForGoTypesTestResult(t, second, nil)
}

func TestGoTypesHeavyGateReleasedOnApplyGateCancellation(t *testing.T) {
	provider, root, loads := newApplyGateTestProvider(t)
	applyGate := make(chan struct{})
	blockedCtx, cancelBlocked := context.WithCancel(semantic.WithApplyGate(context.Background(), applyGate))
	blocked := runApplyGateTestEnrichment(provider, blockedCtx, "blocked", root)

	waitForGoTypesTestLoad(t, loads)
	cancelBlocked()
	waitForGoTypesTestResult(t, blocked, context.Canceled)

	nextCtx, cancelNext := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelNext()
	next := runApplyGateTestEnrichment(provider, nextCtx, "next", root)
	waitForGoTypesTestLoad(t, loads)
	waitForGoTypesTestResult(t, next, nil)
}

func newApplyGateTestProvider(t *testing.T) (*Provider, string, chan struct{}) {
	t.Helper()
	t.Setenv("GORTEX_GOTYPES_NEEDDEPS", "1")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/applygate\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	loads := make(chan struct{}, 2)
	provider := NewProvider(ModeTypeCheck, false, zap.NewNop())
	provider.heavyGate = make(chan struct{}, 1)
	provider.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) {
		loads <- struct{}{}
		return nil, nil
	}
	return provider, root, loads
}

func runApplyGateTestEnrichment(provider *Provider, ctx context.Context, prefix, root string) <-chan error {
	result := make(chan error, 1)
	go func() {
		_, err := provider.enrichRepoContext(ctx, graph.New(), prefix, root)
		result <- err
	}()
	return result
}

func waitForGoTypesTestLoad(t *testing.T, loads <-chan struct{}) {
	t.Helper()
	select {
	case <-loads:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for package load")
	}
}

func waitForGoTypesTestResult(t *testing.T, result <-chan error, want error) {
	t.Helper()
	select {
	case got := <-result:
		if !errors.Is(got, want) {
			t.Fatalf("enrichment error = %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for enrichment")
	}
}
