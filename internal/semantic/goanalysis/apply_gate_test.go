package goanalysis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
)

// detachedApplyStore exposes the repository binding replacement as a precise
// apply boundary. Its embedded store still supplies the real ResolveMutex and
// graph operations used by the rest of enrichment.
type detachedApplyStore struct {
	graph.Store
	entered       chan struct{}
	release       <-chan struct{}
	beforeReplace func()
	enteredOnce   sync.Once
}

func (s *detachedApplyStore) ReplaceSemanticBindingTypes(string, []graph.SemanticBindingType) error {
	if s.beforeReplace != nil {
		s.beforeReplace()
	}
	s.enteredOnce.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func (*detachedApplyStore) ReplaceSemanticBindingTypesForFiles(
	string,
	[]string,
	[]graph.SemanticBindingType,
) error {
	return nil
}

func (*detachedApplyStore) DeleteSemanticBindingTypesByFiles(string, []string) error {
	return nil
}

func TestCanceledDetachedApplyDoesNotSerializeAnotherStore(t *testing.T) {
	provider := newDetachedApplyTestProvider(t)
	firstRoot := newDetachedApplyTestRoot(t, "example.com/first")
	secondRoot := newDetachedApplyTestRoot(t, "example.com/second")
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	firstStore := &detachedApplyStore{
		Store:   graph.New(),
		entered: make(chan struct{}),
		release: firstRelease,
	}
	secondStore := &detachedApplyStore{
		Store:   graph.New(),
		entered: make(chan struct{}),
		release: secondRelease,
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := runDetachedApplyTestEnrichment(
		provider, firstCtx, firstStore, "first", firstRoot,
	)
	waitForDetachedApplyStore(t, firstStore.entered)
	cancelFirst()

	secondResult := runDetachedApplyTestEnrichment(
		provider, context.Background(), secondStore, "second", secondRoot,
	)
	waitForDetachedApplyStore(t, secondStore.entered)

	close(secondRelease)
	close(firstRelease)
	require.ErrorIs(t, waitForDetachedApplyResult(t, firstResult), context.Canceled)
	require.NoError(t, waitForDetachedApplyResult(t, secondResult))
}

func TestCompilerAdmissionReleasedBeforeDetachedStoreMutation(t *testing.T) {
	provider := newDetachedApplyTestProvider(t)
	root := newDetachedApplyTestRoot(t, "example.com/compiler-release")
	storeRelease := make(chan struct{})
	admissionResult := make(chan error, 1)
	store := &detachedApplyStore{
		Store:   graph.New(),
		entered: make(chan struct{}),
		release: storeRelease,
		beforeReplace: func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err := provider.acquireHeavy(ctx, true)
			if release != nil {
				release()
			}
			admissionResult <- err
		},
	}

	result := runDetachedApplyTestEnrichment(
		provider, context.Background(), store, "compiler-release", root,
	)
	select {
	case err := <-admissionResult:
		require.NoError(t, err, "compiler admission must be returned before graph mutation")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out checking compiler admission at the graph mutation boundary")
	}
	close(storeRelease)
	require.NoError(t, waitForDetachedApplyResult(t, result))
}

func newDetachedApplyTestProvider(t *testing.T) *Provider {
	t.Helper()
	t.Setenv("GORTEX_GOTYPES_NEEDDEPS", "1")
	provider := NewProvider(ModeTypeCheck, false, nil)
	provider.heavyGate = make(chan struct{}, 1)
	provider.largeGate = make(chan struct{}, 1)
	provider.compilerGC = func() {}
	provider.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) {
		return nil, nil
	}
	return provider
}

func newDetachedApplyTestRoot(t *testing.T, module string) string {
	t.Helper()
	root := resolvedTempDir(t)
	writeGoMod(t, root, module)
	return root
}

func runDetachedApplyTestEnrichment(
	provider *Provider,
	ctx context.Context,
	store graph.Store,
	prefix, root string,
) <-chan error {
	result := make(chan error, 1)
	go func() {
		_, err := provider.enrichRepoContext(ctx, store, prefix, root)
		result <- err
	}()
	return result
}

func waitForDetachedApplyStore(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for detached graph mutation")
	}
}

func waitForDetachedApplyResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for detached enrichment")
		return nil
	}
}
