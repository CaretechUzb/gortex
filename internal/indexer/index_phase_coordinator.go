package indexer

import (
	"context"
	"errors"
	"sync"
)

// indexPhaseCoordinator orders the two repository-scale phases whose memory
// footprints must not overlap: intrinsically large direct parses and drains of
// pressure-sized in-memory shadows. Ordinary parses and ordinary shadow drains
// never enter this coordinator.
//
// Direct work registers intent before waiting on largeDirectAdmission, but it
// acquires the existing admission lease before beginning this phase. When a
// direct owner releases, the coordinator freezes the then-existing drain
// reservation high-water. Those drains run FIFO before one direct intent that
// was already queued; later drains cannot extend the frozen batch and starve
// that direct.
type indexPhaseCoordinator struct {
	mu sync.Mutex

	nextSequence uint64
	active       *indexPhaseReservation
	reservations []*indexPhaseReservation

	priorityCycle         bool
	frozenDrainHighWater  uint64
	frozenDirectHighWater uint64
}

type indexPhaseKind uint8

const (
	indexPhaseDirect indexPhaseKind = iota + 1
	indexPhaseShadowDrain
)

type indexPhaseReservation struct {
	coordinator *indexPhaseCoordinator
	kind        indexPhaseKind
	sequence    uint64
	notify      chan struct{}

	ready    bool
	granted  bool
	canceled bool
	done     bool
	signaled bool
}

type indexPhaseLease struct {
	reservation *indexPhaseReservation
	once        sync.Once
}

type indexPhaseSnapshot struct {
	activeKind    indexPhaseKind
	directQueued  int
	drainQueued   int
	priorityCycle bool
}

var (
	processIndexPhaseCoordinator     = newIndexPhaseCoordinator()
	errIndexPhaseReservationCanceled = errors.New("indexer: phase reservation canceled")
)

func newIndexPhaseCoordinator() *indexPhaseCoordinator {
	return &indexPhaseCoordinator{}
}

func (c *indexPhaseCoordinator) reserveDirect(weight int64) *indexPhaseReservation {
	if c == nil || weight <= 0 {
		return nil
	}
	return c.reserve(indexPhaseDirect)
}

func (c *indexPhaseCoordinator) reserveShadowDrain(pressureSized bool) *indexPhaseReservation {
	if c == nil || !pressureSized {
		return nil
	}
	return c.reserve(indexPhaseShadowDrain)
}

func (c *indexPhaseCoordinator) reserve(kind indexPhaseKind) *indexPhaseReservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextSequence++
	reservation := &indexPhaseReservation{
		coordinator: c,
		kind:        kind,
		sequence:    c.nextSequence,
		notify:      make(chan struct{}),
	}
	c.reservations = append(c.reservations, reservation)
	return reservation
}

// Begin marks a reservation ready and waits for its phase. A reservation may
// be made before another admission gate is acquired; readiness is separate so
// a direct intent can be visible to the release-time high-water freeze without
// reversing the required largeDirectAdmission -> phase acquisition order.
func (r *indexPhaseReservation) Begin(ctx context.Context) (*indexPhaseLease, error) {
	if r == nil || r.coordinator == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		r.Cancel()
		return nil, err
	}

	c := r.coordinator
	c.mu.Lock()
	if r.canceled || r.done {
		c.mu.Unlock()
		return nil, errIndexPhaseReservationCanceled
	}
	r.ready = true
	c.scheduleLocked()
	if r.granted {
		c.mu.Unlock()
		return &indexPhaseLease{reservation: r}, nil
	}
	notify := r.notify
	c.mu.Unlock()

	select {
	case <-notify:
		c.mu.Lock()
		granted := r.granted && !r.done && !r.canceled
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			if granted {
				c.cancelGranted(r)
			}
			return nil, err
		}
		if !granted {
			return nil, errIndexPhaseReservationCanceled
		}
		return &indexPhaseLease{reservation: r}, nil
	case <-ctx.Done():
		c.mu.Lock()
		if r.granted && !r.done && c.active == r {
			c.releaseLocked(r, true)
		} else if !r.done && !r.canceled {
			r.canceled = true
			c.signalLocked(r)
			c.scheduleLocked()
			c.compactLocked()
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Cancel withdraws an unused reservation. Once Begin has returned a lease, the
// lease owns release; Cancel deliberately becomes a no-op to keep ownership
// exactly once under error and panic cleanup defers.
func (r *indexPhaseReservation) Cancel() {
	if r == nil || r.coordinator == nil {
		return
	}
	c := r.coordinator
	c.mu.Lock()
	if !r.granted && !r.done && !r.canceled {
		r.canceled = true
		c.signalLocked(r)
		c.scheduleLocked()
		c.compactLocked()
	}
	c.mu.Unlock()
}

func (l *indexPhaseLease) Release() {
	if l == nil || l.reservation == nil || l.reservation.coordinator == nil {
		return
	}
	l.once.Do(func() {
		c := l.reservation.coordinator
		c.mu.Lock()
		c.releaseLocked(l.reservation, false)
		c.mu.Unlock()
	})
}

func (c *indexPhaseCoordinator) cancelGranted(r *indexPhaseReservation) {
	c.mu.Lock()
	if r.granted && !r.done && c.active == r {
		c.releaseLocked(r, true)
	}
	c.mu.Unlock()
}

func (c *indexPhaseCoordinator) releaseLocked(r *indexPhaseReservation, canceled bool) {
	if r == nil || r.done || c.active != r {
		return
	}
	c.active = nil
	r.done = true
	r.canceled = r.canceled || canceled

	if r.kind == indexPhaseDirect {
		// The direct intent queue is populated before callers wait on the old
		// large-direct semaphore. Snapshot both high-waters before that
		// semaphore is released: drains already complete enough to reserve may
		// overtake one already-waiting direct, while later drains may not.
		directHighWater := c.highWaterLocked(indexPhaseDirect)
		if directHighWater > 0 {
			c.priorityCycle = true
			c.frozenDrainHighWater = c.highWaterLocked(indexPhaseShadowDrain)
			c.frozenDirectHighWater = directHighWater
		} else {
			c.clearPriorityCycleLocked()
		}
	}
	c.scheduleLocked()
	c.compactLocked()
}

func (c *indexPhaseCoordinator) scheduleLocked() {
	if c.active != nil {
		return
	}

	if c.priorityCycle {
		if drain := c.firstLocked(indexPhaseShadowDrain, c.frozenDrainHighWater, false); drain != nil {
			// FIFO means an earlier frozen reservation also owns readiness:
			// do not let a later drain or the direct turn pass it while its
			// successful parse finishes the small pre-drain tail.
			if !drain.ready {
				return
			}
			c.grantLocked(drain)
			return
		}
		if direct := c.firstLocked(indexPhaseDirect, c.frozenDirectHighWater, false); direct != nil {
			if !direct.ready {
				return
			}
			// One queued direct ends this priority cycle. Its eventual release
			// freezes a fresh high-water for the next fair handoff.
			c.clearPriorityCycleLocked()
			c.grantLocked(direct)
			return
		}
		c.clearPriorityCycleLocked()
	}

	// Outside a frozen handoff, run the oldest ready reservation. An unready
	// intent never idles the phase unless a direct release explicitly froze it
	// into the next handoff.
	var next *indexPhaseReservation
	for _, reservation := range c.reservations {
		if reservation == nil || reservation.canceled || reservation.done || reservation.granted || !reservation.ready {
			continue
		}
		if next == nil || reservation.sequence < next.sequence {
			next = reservation
		}
	}
	if next != nil {
		c.grantLocked(next)
	}
}

func (c *indexPhaseCoordinator) grantLocked(r *indexPhaseReservation) {
	c.active = r
	r.granted = true
	c.signalLocked(r)
}

func (c *indexPhaseCoordinator) signalLocked(r *indexPhaseReservation) {
	if r == nil || r.signaled {
		return
	}
	r.signaled = true
	close(r.notify)
}

func (c *indexPhaseCoordinator) firstLocked(kind indexPhaseKind, highWater uint64, readyOnly bool) *indexPhaseReservation {
	if highWater == 0 {
		return nil
	}
	var first *indexPhaseReservation
	for _, reservation := range c.reservations {
		if reservation == nil || reservation.kind != kind || reservation.sequence > highWater ||
			reservation.canceled || reservation.done || reservation.granted || (readyOnly && !reservation.ready) {
			continue
		}
		if first == nil || reservation.sequence < first.sequence {
			first = reservation
		}
	}
	return first
}

func (c *indexPhaseCoordinator) highWaterLocked(kind indexPhaseKind) uint64 {
	var highWater uint64
	for _, reservation := range c.reservations {
		if reservation == nil || reservation.kind != kind || reservation.canceled || reservation.done || reservation.granted {
			continue
		}
		if reservation.sequence > highWater {
			highWater = reservation.sequence
		}
	}
	return highWater
}

func (c *indexPhaseCoordinator) clearPriorityCycleLocked() {
	c.priorityCycle = false
	c.frozenDrainHighWater = 0
	c.frozenDirectHighWater = 0
}

// compactLocked drops terminal reservations once no scheduler decision can
// reference them. Long-lived daemons otherwise retain one small coordinator
// object per repository-scale phase forever.
func (c *indexPhaseCoordinator) compactLocked() {
	kept := c.reservations[:0]
	for _, reservation := range c.reservations {
		if reservation == nil || ((reservation.done || reservation.canceled) && c.active != reservation) {
			continue
		}
		kept = append(kept, reservation)
	}
	for i := len(kept); i < len(c.reservations); i++ {
		c.reservations[i] = nil
	}
	c.reservations = kept
}

func (c *indexPhaseCoordinator) snapshot() indexPhaseSnapshot {
	if c == nil {
		return indexPhaseSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := indexPhaseSnapshot{priorityCycle: c.priorityCycle}
	if c.active != nil {
		result.activeKind = c.active.kind
	}
	for _, reservation := range c.reservations {
		if reservation == nil || reservation.canceled || reservation.done || reservation.granted {
			continue
		}
		switch reservation.kind {
		case indexPhaseDirect:
			result.directQueued++
		case indexPhaseShadowDrain:
			result.drainQueued++
		}
	}
	return result
}

// largeDirectParsePhaseLease preserves the old large-direct gate while adding
// the ordering phase. Release is deliberately phase-first: that freezes drain
// reservations while the next direct intent is still visible behind the old
// semaphore, then wakes that direct so it can claim its reserved turn.
type largeDirectParsePhaseLease struct {
	direct *largeDirectAdmissionLease
	phase  *indexPhaseLease
	once   sync.Once
}

func acquireLargeDirectParsePhase(
	ctx context.Context,
	budget *largeDirectAdmissionBudget,
	coordinator *indexPhaseCoordinator,
	weight int64,
) (*largeDirectParsePhaseLease, error) {
	if weight <= 0 {
		return nil, nil
	}
	intent := coordinator.reserveDirect(weight)
	direct, err := budget.acquire(ctx, weight)
	if err != nil {
		intent.Cancel()
		return nil, err
	}
	phase, err := intent.Begin(ctx)
	if err != nil {
		direct.Release()
		return nil, err
	}
	return &largeDirectParsePhaseLease{direct: direct, phase: phase}, nil
}

func (l *largeDirectParsePhaseLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.phase != nil {
			l.phase.Release()
		}
		if l.direct != nil {
			l.direct.Release()
		}
	})
}
