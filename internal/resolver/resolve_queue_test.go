package resolver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// unresolvedCallGraph builds the smallest graph whose ResolveAllContext has real
// work to do, so the pass reaches the body of the function rather than
// short-circuiting on an empty pending set.
func unresolvedCallGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "repoB/target.go::Helper", Kind: graph.KindFunction, Name: "Helper",
		FilePath: "repoB/target.go", Language: "go", RepoPrefix: "repoB",
	})
	g.AddNode(&graph.Node{
		ID: "repoA/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "repoA/caller.go", Language: "go", RepoPrefix: "repoA",
	})
	g.AddEdge(&graph.Edge{
		From: "repoA/caller.go::Caller", To: "unresolved::Helper",
		Kind: graph.EdgeCalls, FilePath: "repoA/caller.go", Line: 1,
	})
	return g
}

// holdResolveMutex takes the store-wide resolve mutex and releases it after
// hold. Returns once the mutex is actually held, so the caller can start a pass
// knowing it will queue.
func holdResolveMutex(g graph.Store, hold time.Duration) {
	acquired := make(chan struct{})
	go func() {
		mu := g.ResolveMutex()
		mu.Lock()
		close(acquired)
		time.Sleep(hold)
		mu.Unlock()
	}()
	<-acquired
}

// TestCrossRepoResolveAllAnnouncesItsQueueWait is the regression gate for the
// silent stall. Before this, a cross-repo resolve blocked on the store-wide
// resolve mutex logged NOTHING until it acquired: "pass start" is inside the
// lock, so a pass queued behind a semantic enrichment apply left a gap in the
// daemon log — measured once at 24m45s — that read exactly like a wedged
// daemon.
func TestCrossRepoResolveAllAnnouncesItsQueueWait(t *testing.T) {
	restore := resolveQueueLogThreshold
	resolveQueueLogThreshold = 5 * time.Millisecond
	t.Cleanup(func() { resolveQueueLogThreshold = restore })

	core, logs := observer.New(zap.InfoLevel)
	g := unresolvedCallGraph()
	cr := NewCrossRepo(g)
	cr.SetLogger(zap.New(core))

	const hold = 120 * time.Millisecond
	holdResolveMutex(g, hold)

	start := time.Now()
	_, err := cr.ResolveAllContext(context.Background())
	require.NoError(t, err)

	queued := logs.FilterMessage("cross-repo resolve: pass began after queueing").All()
	require.Len(t, queued, 1, "the queue wait must be announced exactly once")
	for _, field := range queued[0].Context {
		if field.Key == "queued" {
			// Most of the hold, not all of it: holdResolveMutex returns the
			// instant the mutex is taken and the sleep starts there, so the pass
			// below necessarily begins waiting a little after — the reported
			// wait is shorter than the hold by that gap, by construction.
			// The claim under test is that a REAL wait is reported, not a
			// rounding of one.
			require.Greater(t, time.Duration(field.Integer), hold/2,
				"the reported wait must be the real one")
			require.Greater(t, time.Since(start), hold/2,
				"the pass must have actually waited for the mutex")
			return
		}
	}
	t.Fatal("the queueing line carried no queued duration")
}

// TestMasterResolveAllAnnouncesItsQueueWait is the same gate on the master
// resolver, which shares the mutex and had the same blind spot.
func TestMasterResolveAllAnnouncesItsQueueWait(t *testing.T) {
	restore := resolveQueueLogThreshold
	resolveQueueLogThreshold = 5 * time.Millisecond
	t.Cleanup(func() { resolveQueueLogThreshold = restore })

	core, logs := observer.New(zap.InfoLevel)
	g := unresolvedCallGraph()
	r := New(g)
	r.SetLogger(zap.New(core))

	holdResolveMutex(g, 120*time.Millisecond)

	_, err := r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	require.Len(t, logs.FilterMessage("resolver: pass began after queueing").All(), 1)
}

// TestResolveQueueIsQuietBelowTheThreshold keeps the line from becoming noise:
// an uncontended pass — every ordinary one — must say nothing at all.
func TestResolveQueueIsQuietBelowTheThreshold(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	g := unresolvedCallGraph()
	cr := NewCrossRepo(g)
	cr.SetLogger(zap.New(core))

	_, err := cr.ResolveAllContext(context.Background())
	require.NoError(t, err)
	require.Empty(t, logs.FilterMessage("cross-repo resolve: pass began after queueing").All())
}

// TestResolveQueueWaitsIsVisibleWhileWaiting covers the status surface. The log
// line lands only once the wait is OVER, which is precisely too late for a
// reader asking why the daemon looks idle, so the queue has to be readable
// while it is still happening.
func TestResolveQueueWaitsIsVisibleWhileWaiting(t *testing.T) {
	require.Empty(t, ResolveQueueWaits(), "the queue must start empty")

	g := unresolvedCallGraph()
	cr := NewCrossRepo(g)
	holdResolveMutex(g, 300*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cr.ResolveAllContext(context.Background())
	}()

	var seen []ResolveQueueWait
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if seen = ResolveQueueWaits(); len(seen) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.Len(t, seen, 1, "the blocked pass must be visible while it waits")
	require.Equal(t, "cross-repo resolve", seen[0].Pass)

	queued, pass := LongestResolveQueueWait(time.Now())
	require.Equal(t, "cross-repo resolve", pass)
	require.Positive(t, queued)

	<-done
	require.Empty(t, ResolveQueueWaits(), "the wait must be retired once acquired")
}
