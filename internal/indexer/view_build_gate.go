package indexer

import "sync"

// The warmup gate over view build work.
//
// A warm restart brings everything back at once. The warmup tail re-resolves
// the graph and runs the whole-graph passes, and at the same moment every
// automatic checkout the catalog remembers wants its two layers built and
// every ref view somebody selects wants a pass of its own. All of it goes
// through one store writer and one process-global topology gate, so a restart
// over a fully persisted graph can spend longer serializing view builds behind
// the warmup than a cold index spends producing the graph in the first place.
//
// The gate defers the build half of that and nothing else. Registration,
// catalog seeding, route reads and serving a generation that is already
// published are not build work: they never consult the gate, so a warming
// daemon keeps answering from whatever the last run left behind.
//
// A closed gate does not drop the work it holds. A coordinator cycle that
// finds it closed reschedules itself onto the gate's own wake, and a ref
// view's build keeps the claim it made — so the selection that made it is
// still answered with a token to poll, later selections still coalesce onto
// that token, and the pass runs the moment builds are admitted. Nothing has to
// ask again.
type ViewBuildGate struct {
	mu     sync.Mutex
	open   bool
	opened chan struct{}
}

// NewViewBuildGate returns a gate that holds build work until Open.
func NewViewBuildGate() *ViewBuildGate {
	return &ViewBuildGate{opened: make(chan struct{})}
}

// Open admits build work and wakes everything waiting on it. It is idempotent:
// the daemon's warmup opens the gate once, and a second call from any other
// path is a no-op rather than a double close.
func (g *ViewBuildGate) Open() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open {
		return
	}
	g.open = true
	close(g.opened)
}

// Admitted reports whether a build may start now.
//
// A nil gate admits everything. That is what every caller outside a daemon
// warmup has — an embedded server, a CLI pass, a test — and none of them has a
// warmup tail to defer to.
func (g *ViewBuildGate) Admitted() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

// Opened is closed once builds are admitted. A caller that has already found
// Admitted false waits on it; one that has not must not select on it, because
// an open gate's channel is always ready and would spin a loop that treats it
// as a wake.
func (g *ViewBuildGate) Opened() <-chan struct{} {
	if g == nil {
		return admittedChannel
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.opened
}

// admittedChannel is what a nil gate's Opened answers with: already open.
var admittedChannel = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()
