package indexer

import (
	"context"
	"errors"
	"testing"
	"time"
)

const repositoryMutationAdmissionTestTimeout = 2 * time.Second

func waitForRepositoryMutationLaneTokens(
	t *testing.T,
	coordinator *repositoryMutationCoordinator,
	want int,
) {
	t.Helper()
	deadline := time.NewTimer(repositoryMutationAdmissionTestTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := len(coordinator.lane); got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("repository mutation lane token count = %d, want %d", len(coordinator.lane), want)
		case <-ticker.C:
		}
	}
}

func TestWithRepositoryMutationLanesCancellationUnwindsSortedLanes(t *testing.T) {
	mi := &MultiIndexer{}
	earlier := mi.repositoryMutationCoordinator("a")
	later := mi.repositoryMutationCoordinator("z")

	// Hold the later lane. Passing the prefixes in reverse order proves that
	// withRepositoryMutationLanes sorts before acquisition: it must take "a"
	// before blocking on "z".
	<-later.lane
	laterHeld := true
	t.Cleanup(func() {
		if laterHeld {
			later.lane <- struct{}{}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callbackCalled := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- mi.withRepositoryMutationLanes(ctx, []string{"z", "a", "z"}, func() error {
			callbackCalled <- struct{}{}
			return nil
		})
	}()

	waitForRepositoryMutationLaneTokens(t, earlier, 0)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled multi-lane acquisition error = %v, want context.Canceled", err)
		}
	case <-time.After(repositoryMutationAdmissionTestTimeout):
		t.Fatal("cancelled multi-lane acquisition did not return")
	}
	select {
	case <-callbackCalled:
		t.Fatal("multi-lane callback ran without acquiring the held later lane")
	default:
	}
	if got := len(earlier.lane); got != 1 {
		t.Fatalf("earlier lane token count after cancellation = %d, want 1", got)
	}

	later.lane <- struct{}{}
	laterHeld = false
	callbackRan := false
	if err := mi.withRepositoryMutationLanes(
		context.Background(),
		[]string{"z", "a", "z"},
		func() error {
			callbackRan = true
			return nil
		},
	); err != nil {
		t.Fatalf("reusing lanes after cancellation: %v", err)
	}
	if !callbackRan {
		t.Fatal("multi-lane callback did not run after cancelled acquisition unwound")
	}
}

type repositorySnapshotAdmissionResult struct {
	coordinator *repositoryMutationCoordinator
	current     bool
}

func TestRepositoryMutationSnapshotAdmissionDoesNotCrossRetrackGeneration(t *testing.T) {
	for _, tc := range []struct {
		name            string
		attachOld       bool
		wantCurrent     bool
		wantCoordinator bool
	}{
		{
			name:            "attached snapshot keeps closed old generation",
			attachOld:       true,
			wantCurrent:     true,
			wantCoordinator: true,
		},
		{
			name:        "unattached snapshot backstop rejects replacement",
			attachOld:   false,
			wantCurrent: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prefix = "same-prefix"
			mi := &MultiIndexer{
				repos:    make(map[string]*RepoMetadata),
				indexers: make(map[string]*Indexer),
			}
			oldRoot := t.TempDir()
			replacementRoot := t.TempDir()
			oldMeta := &RepoMetadata{RepoPrefix: prefix, RootPath: oldRoot}
			oldIndexer := &Indexer{}
			oldCoordinator := mi.repositoryMutationCoordinator(prefix)
			if tc.attachOld {
				oldIndexer.attachRepositoryMutationCoordinator(oldCoordinator)
			}
			mi.repos[prefix] = oldMeta
			mi.indexers[prefix] = oldIndexer

			// Capture the exact metadata and Indexer pointers, then delay admission
			// until the same prefix has been untracked and retracked at another
			// root. This is the ABA window IncrementalReindexRepo must survive.
			captured := make(chan struct{})
			admit := make(chan struct{})
			done := make(chan repositorySnapshotAdmissionResult, 1)
			go func(meta *RepoMetadata, idx *Indexer) {
				close(captured)
				<-admit
				coordinator, current := mi.repositoryMutationCoordinatorForSnapshot(prefix, meta, idx)
				done <- repositorySnapshotAdmissionResult{coordinator: coordinator, current: current}
			}(oldMeta, oldIndexer)
			<-captured

			if err := oldCoordinator.closeAndWait(context.Background()); err != nil {
				t.Fatalf("closing old coordinator: %v", err)
			}
			mi.mu.Lock()
			delete(mi.repos, prefix)
			delete(mi.indexers, prefix)
			mi.mu.Unlock()
			if !mi.detachRepositoryMutationCoordinator(prefix, oldCoordinator) {
				t.Fatal("old coordinator was not detached during simulated untrack")
			}

			replacementCoordinator := mi.repositoryMutationCoordinator(prefix)
			replacementIndexer := &Indexer{}
			replacementIndexer.attachRepositoryMutationCoordinator(replacementCoordinator)
			replacementMeta := &RepoMetadata{RepoPrefix: prefix, RootPath: replacementRoot}
			mi.mu.Lock()
			mi.repos[prefix] = replacementMeta
			mi.indexers[prefix] = replacementIndexer
			mi.mu.Unlock()

			close(admit)
			var admitted repositorySnapshotAdmissionResult
			select {
			case admitted = <-done:
			case <-time.After(repositoryMutationAdmissionTestTimeout):
				t.Fatal("stale snapshot admission did not return")
			}
			if admitted.current != tc.wantCurrent {
				t.Fatalf("stale snapshot current = %v, want %v", admitted.current, tc.wantCurrent)
			}
			if tc.wantCoordinator {
				if admitted.coordinator != oldCoordinator {
					t.Fatalf("stale snapshot coordinator = %p, want old generation %p", admitted.coordinator, oldCoordinator)
				}
			} else if admitted.coordinator != nil {
				t.Fatalf("stale unattached snapshot coordinator = %p, want nil", admitted.coordinator)
			}
			if oldIndexer.attachedRepositoryMutationCoordinator() == replacementCoordinator {
				t.Fatal("stale Indexer was attached to the replacement coordinator")
			}
			if got := replacementIndexer.attachedRepositoryMutationCoordinator(); got != replacementCoordinator {
				t.Fatalf("replacement Indexer coordinator = %p, want %p", got, replacementCoordinator)
			}
			if got, current := mi.repositoryMutationCoordinatorForSnapshot(prefix, replacementMeta, replacementIndexer); !current || got != replacementCoordinator {
				t.Fatalf("replacement snapshot admission = (%p, %v), want (%p, true)", got, current, replacementCoordinator)
			}
		})
	}
}
