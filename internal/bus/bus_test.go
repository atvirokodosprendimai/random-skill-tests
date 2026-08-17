package bus_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/bus"
	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// recvTimeout is generous: these tests assert on ordering and liveness, never
// on speed, so the only thing it guards against is a hung test.
const recvTimeout = 2 * time.Second

// mustRecv returns the next id on ch, failing the test if none arrives or the
// channel is closed first.
func mustRecv(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, want an aggregate id")
		}
		return v
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for a notification")
		return ""
	}
}

// mustNotRecv fails if anything is already queued on ch. It only inspects what
// is buffered, since a notification that has not been published yet cannot be
// distinguished from one that never will be.
func mustNotRecv(t *testing.T, ch <-chan string) {
	t.Helper()
	select {
	case v, ok := <-ch:
		t.Fatalf("unexpected value %q (open=%t), want nothing queued", v, ok)
	default:
	}
}

func TestPublishDeliversAggregateID(t *testing.T) {
	t.Parallel()

	b := bus.New()
	defer b.Close()

	ch, unsub := b.Subscribe(core.SubjectTimer)
	defer unsub()

	b.Publish(core.SubjectTimer, core.TimerAggregate)

	if got := mustRecv(t, ch); got != core.TimerAggregate {
		t.Fatalf("got %q, want %q", got, core.TimerAggregate)
	}
}

func TestTwoSubscribersBothReceive(t *testing.T) {
	t.Parallel()

	b := bus.New()
	defer b.Close()

	first, unsubFirst := b.Subscribe(core.SubjectProjects)
	defer unsubFirst()
	second, unsubSecond := b.Subscribe(core.SubjectProjects)
	defer unsubSecond()

	want := core.ProjectAggregate("tempo")
	b.Publish(core.SubjectProjects, want)

	if got := mustRecv(t, first); got != want {
		t.Errorf("first subscriber got %q, want %q", got, want)
	}
	if got := mustRecv(t, second); got != want {
		t.Errorf("second subscriber got %q, want %q", got, want)
	}
}

func TestSubjectsAreIndependent(t *testing.T) {
	t.Parallel()

	b := bus.New()
	defer b.Close()

	timer, unsubTimer := b.Subscribe(core.SubjectTimer)
	defer unsubTimer()
	rates, unsubRates := b.Subscribe(core.SubjectRates)
	defer unsubRates()

	b.Publish(core.SubjectTimer, core.TimerAggregate)

	if got := mustRecv(t, timer); got != core.TimerAggregate {
		t.Fatalf("timer subscriber got %q, want %q", got, core.TimerAggregate)
	}
	mustNotRecv(t, rates)

	// And the other way round, to prove neither subject is merely quiet.
	b.Publish(core.SubjectRates, core.RatesAggregate)

	if got := mustRecv(t, rates); got != core.RatesAggregate {
		t.Fatalf("rates subscriber got %q, want %q", got, core.RatesAggregate)
	}
	mustNotRecv(t, timer)
}

// TestPublishNeverBlocksOnUndrainedSubscriber is the reason the buffer is one
// slot deep and lossy: a subscriber that has stopped reading must not be able
// to stall the writer that produced the events.
func TestPublishNeverBlocksOnUndrainedSubscriber(t *testing.T) {
	t.Parallel()

	b := bus.New()
	defer b.Close()

	// Deliberately never drained.
	_, unsub := b.Subscribe(core.SubjectEntries)
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10_000 {
			b.Publish(core.SubjectEntries, core.EntryAggregate(fmt.Sprint(i)))
		}
	}()

	select {
	case <-done:
	case <-time.After(recvTimeout):
		// Reported as a failure rather than left to hang, so a regression
		// shows up as a red test instead of a stuck CI run.
		t.Fatal("Publish blocked on a subscriber that never drains")
	}
}

// TestPublishDropsOldestID pins the drop-oldest policy: when the slot is full
// the superseded id goes, not the fresh one. Delivering the stale id would send
// the subscriber to re-read the log for a state it has already been told about,
// while the newest change went unannounced.
func TestPublishDropsOldestID(t *testing.T) {
	t.Parallel()

	b := bus.New()
	defer b.Close()

	ch, unsub := b.Subscribe(core.SubjectEntries)
	defer unsub()

	b.Publish(core.SubjectEntries, "entry:oldest")
	b.Publish(core.SubjectEntries, "entry:middle")
	b.Publish(core.SubjectEntries, "entry:newest")

	if got := mustRecv(t, ch); got != "entry:newest" {
		t.Fatalf("got %q, want the newest id %q", got, "entry:newest")
	}
	// Capacity one means exactly one id survives; the rest are gone.
	mustNotRecv(t, ch)
}

func TestUnsubscribeIsIdempotentAndStopsDelivery(t *testing.T) {
	t.Parallel()

	b := bus.New()
	defer b.Close()

	ch, unsub := b.Subscribe(core.SubjectTimer)

	unsub()
	unsub() // Must not panic on a double close of the same channel.

	// Publishing to a subject whose only subscriber left is a no-op, and must
	// not send on the now-closed channel.
	b.Publish(core.SubjectTimer, core.TimerAggregate)

	if v, ok := <-ch; ok {
		t.Fatalf("got %q on an unsubscribed channel, want it closed", v)
	}
}

func TestCloseIsSafeForEveryLaterCall(t *testing.T) {
	t.Parallel()

	b := bus.New()
	ch, unsub := b.Subscribe(core.SubjectProjects)

	b.Close()
	b.Close() // Idempotent: the second sweep must close nothing twice.

	if v, ok := <-ch; ok {
		t.Fatalf("got %q after Close, want the channel closed", v)
	}

	// None of these may panic.
	unsub()
	unsub()
	b.Publish(core.SubjectProjects, core.ProjectAggregate("tempo"))
}

// TestSubscribeAfterCloseYieldsClosedChannel keeps a late subscriber from
// blocking forever on a bus that will never publish again: its range loop ends
// immediately, exactly as it would have on shutdown.
func TestSubscribeAfterCloseYieldsClosedChannel(t *testing.T) {
	t.Parallel()

	b := bus.New()
	b.Close()

	ch, unsub := b.Subscribe(core.SubjectRates)
	unsub()
	unsub()

	if v, ok := <-ch; ok {
		t.Fatalf("got %q from a closed bus, want the channel closed", v)
	}
	b.Publish(core.SubjectRates, core.RatesAggregate)
}

// TestConcurrentPublishSubscribeUnsubscribe hammers every entry point at once.
// It exists for the -race detector: the assertions are only that nothing
// panics and that the run terminates, because under this much churn no
// individual delivery is guaranteed — only that publishing stays lock-free of
// subscribers and that no channel is ever closed twice or written after close.
func TestConcurrentPublishSubscribeUnsubscribe(t *testing.T) {
	t.Parallel()

	const (
		publishers = 8
		churners   = 8
		iterations = 500
	)
	subjects := []string{
		core.SubjectProjects,
		core.SubjectTimer,
		core.SubjectEntries,
		core.SubjectRates,
	}

	b := bus.New()

	// A long-lived subscriber that keeps draining, so publishes hit both full
	// and empty buffers throughout the run. Its loop ends when Close closes the
	// channel, which is also the assertion that Close reaches every subscriber.
	drained, _ := b.Subscribe(core.SubjectTimer)
	var drainer sync.WaitGroup
	drainer.Add(1)
	go func() {
		defer drainer.Done()
		for range drained {
		}
	}()

	stop := make(chan struct{})
	var publishing sync.WaitGroup
	for p := range publishers {
		publishing.Add(1)
		go func() {
			defer publishing.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				b.Publish(subjects[n%len(subjects)], fmt.Sprintf("agg:%d:%d", p, n))
			}
		}()
	}

	var churning sync.WaitGroup
	for c := range churners {
		churning.Add(1)
		go func() {
			defer churning.Done()
			for n := range iterations {
				ch, unsub := b.Subscribe(subjects[(c+n)%len(subjects)])
				select {
				case <-ch:
				default:
				}
				unsub()
				unsub() // Idempotent even while publishers are hammering.
			}
		}()
	}

	churning.Wait()
	close(stop)
	publishing.Wait()

	b.Close()
	drainer.Wait()

	// Publishing into the wreckage must still be a silent no-op.
	b.Publish(core.SubjectTimer, core.TimerAggregate)
}
