// Package bus is tempo's in-process fan-out of change notifications.
//
// A writer that has just appended to the log calls Publish with the aggregate
// id it touched; every subscriber to that subject wakes up and re-reads the log
// itself. The value on the wire is only ever an id, never rendered state, which
// is what makes this bus allowed to be lossy: if two notifications for one
// aggregate collide, the second one supersedes the first completely, and a
// subscriber that acts on the second reads strictly fresher state than one that
// acted on both. See core.Publisher for the same argument from the port side.
//
// That property drives the whole implementation. Subscriber channels are
// buffered with capacity one and drop the older id when full, so Publish never
// blocks behind a slow subscriber and no writer can ever be back-pressured by a
// reader that wandered off.
package bus

import (
	"sync"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// Bus is the in-process implementation of core.Publisher. A writer holding its
// own lock must be able to publish without risking a deadlock, so this
// assertion is more than paperwork: it pins Bus to the narrow, non-blocking
// contract that port promises.
var _ core.Publisher = (*Bus)(nil)

// Bus is an in-process fan-out of aggregate-id notifications.
//
// The zero value is not usable; call New. A Bus is safe for concurrent use by
// any number of goroutines, and every method is safe to call after Close.
type Bus struct {
	// mu guards subs and closed.
	//
	// A plain mutex, deliberately, rather than a registry goroutine reached
	// over a channel. Publish is called from write models that are already
	// holding their own lock when they append; a channel handshake would make
	// Publish's completion depend on the registry goroutine, which in turn can
	// be blocked behind whatever else that goroutine is doing — a lock-order
	// inversion that deadlocks the writer. A mutex held only for a map lookup
	// and a handful of non-blocking sends can never wait on anything, so it
	// cannot participate in a cycle.
	mu sync.Mutex

	// subs maps subject to the set of live subscriber channels. Membership is
	// the ownership token for a channel: whoever removes an entry from this map
	// is the one that closes it, which makes a double close impossible by
	// construction rather than by convention.
	subs map[string]map[chan string]struct{}

	// closed records that Close has run, so that a later Publish is a silent
	// no-op and a second Close does not close anything twice.
	closed bool
}

// New returns a ready Bus.
func New() *Bus {
	return &Bus{subs: make(map[string]map[chan string]struct{})}
}

// Publish notifies every subscriber of subject that aggregateID changed.
//
// It never blocks and never panics: a subscriber whose buffer is still full
// has its pending id replaced by the newer one, a subject with no subscribers
// costs a single map lookup, and a call after Close does nothing at all.
func (b *Bus) Publish(subject, aggregateID string) {
	// The lock is held across the sends, which is safe only because every send
	// below is non-blocking. No subscriber can extend this critical section.
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	for ch := range b.subs[subject] {
		send(ch, aggregateID)
	}
}

// Subscribe registers for subject, returning a receive channel and an
// unsubscribe func.
//
// The channel is buffered with capacity one and carries aggregate ids. It is
// closed when the subscriber unsubscribes or the Bus is closed, so a range loop
// over it terminates on shutdown. Subscribing to an already-closed Bus yields
// an already-closed channel and a no-op unsubscribe, for the same reason.
//
// unsubscribe is idempotent: calling it twice, or calling it after Close, does
// nothing the second time.
func (b *Bus) Subscribe(subject string) (<-chan string, func()) {
	ch := make(chan string, 1)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		close(ch)
		return ch, func() {}
	}
	set, ok := b.subs[subject]
	if !ok {
		set = make(map[chan string]struct{})
		b.subs[subject] = set
	}
	set[ch] = struct{}{}

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.removeLocked(subject, ch)
	}
}

// Close unsubscribes everyone and closes every subscriber channel.
//
// It is idempotent, and subsequent Publish calls are silent no-ops.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true
	for subject, set := range b.subs {
		for ch := range set {
			close(ch)
		}
		delete(b.subs, subject)
	}
}

// removeLocked deregisters ch from subject and closes it, but only if it is
// still registered. b.mu must be held.
//
// The membership check is the whole idempotency story: an unsubscribe func
// called twice, or called after Close already swept the map, finds nothing to
// remove and therefore closes nothing. Closing under the same mutex that
// Publish holds is also what guarantees Publish never sends on a closed
// channel — a channel is unreachable from subs the instant before it is closed.
func (b *Bus) removeLocked(subject string, ch chan string) {
	set, ok := b.subs[subject]
	if !ok {
		return
	}
	if _, ok := set[ch]; !ok {
		return
	}
	delete(set, ch)
	close(ch)
	if len(set) == 0 {
		// Keep the map from accumulating empty sets for subjects nobody
		// watches any more; a long-lived Bus outlives many subscribers.
		delete(b.subs, subject)
	}
}

// send delivers aggregateID to ch without ever blocking, dropping the oldest
// pending id if the single-slot buffer is still full.
//
// Drop-oldest is correct here and queueing would not be: the payload is an
// aggregate id, so a pending notification that a newer one supersedes tells the
// subscriber nothing extra — it is going to re-read the log and see the newest
// state either way. Growing a buffer instead would only delay the same read
// while spending memory on ids that are already stale.
func send(ch chan string, aggregateID string) {
	select {
	case ch <- aggregateID:
	default:
		// Full: discard the superseded id, then hand over the new one.
		select {
		case <-ch:
		default:
			// The subscriber drained between the two selects, so the slot is
			// already free. Nothing to discard.
		}
		select {
		case ch <- aggregateID:
		default:
			// Unreachable while b.mu is held: only Publish fills this buffer,
			// and the slot was just freed. The non-blocking form keeps "Publish
			// never blocks" a property of the code rather than of an argument
			// about who holds which lock.
		}
	}
}
