package eventstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// baseTime is a fixed, non-UTC instant with sub-second precision. Non-UTC
// catches a Store that forgets to normalise, and the odd nanosecond count
// catches one that truncates on the way through RFC3339Nano.
var baseTime = time.Date(2026, 3, 14, 9, 26, 53, 589793238, time.FixedZone("CET", 2*60*60))

// newStore opens a file-backed store in a temp directory and closes it when the
// test ends. File-backed rather than in-memory because that is what production
// does; the in-memory path gets its own test.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "log.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// event builds a valid event with a caller-chosen ID, so that tests can force
// the duplicate-ID collision that core.NewEvent's random IDs would never hit.
func event(id string, kind core.Kind, aggregate string, at time.Time) core.Event {
	return core.Event{
		ID:        id,
		Kind:      kind,
		Aggregate: aggregate,
		At:        at,
		Payload:   json.RawMessage(`{"slug":"acme","name":"Acme"}`),
	}
}

// ids returns the event IDs in order, which is what most assertions compare:
// it names the mismatch instead of dumping two structs.
func ids(events []core.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

// mustLoad loads from seq 0 and fails the test on error.
func mustLoad(t *testing.T, s *Store, sinceSeq int64) []core.Event {
	t.Helper()
	got, err := s.Load(t.Context(), sinceSeq)
	if err != nil {
		t.Fatalf("Load(%d): %v", sinceSeq, err)
	}
	if got == nil {
		t.Fatalf("Load(%d) returned a nil slice; the contract is non-nil", sinceSeq)
	}
	return got
}

func TestAppendLoadRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	want := []core.Event{
		event("a1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime),
		event("a2", core.KindTimerStarted, core.TimerAggregate, baseTime.Add(time.Minute)),
		event("a3", core.KindTimerStopped, core.TimerAggregate, baseTime.Add(time.Hour)),
	}
	if err := s.Append(t.Context(), want...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got := mustLoad(t, s, 0)
	if len(got) != len(want) {
		t.Fatalf("Load returned %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Seq != int64(i+1) {
			t.Errorf("event %d: Seq = %d, want %d", i, got[i].Seq, i+1)
		}
		if got[i].ID != want[i].ID {
			t.Errorf("event %d: ID = %q, want %q", i, got[i].ID, want[i].ID)
		}
		if got[i].Kind != want[i].Kind {
			t.Errorf("event %d: Kind = %q, want %q", i, got[i].Kind, want[i].Kind)
		}
		if got[i].Aggregate != want[i].Aggregate {
			t.Errorf("event %d: Aggregate = %q, want %q", i, got[i].Aggregate, want[i].Aggregate)
		}
		if !got[i].At.Equal(want[i].At) {
			t.Errorf("event %d: At = %v, want %v", i, got[i].At, want[i].At)
		}
		if got[i].At.Location() != time.UTC {
			t.Errorf("event %d: At location = %v, want UTC", i, got[i].At.Location())
		}
		if string(got[i].Payload) != string(want[i].Payload) {
			t.Errorf("event %d: Payload = %s, want %s", i, got[i].Payload, want[i].Payload)
		}
	}
}

func TestAppendSeqStrictlyIncreasesAcrossCalls(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	// Batches of differing size: seq must be global to the log, not per-call.
	batches := [][]core.Event{
		{event("b1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime)},
		{
			event("b2", core.KindTimerStarted, core.TimerAggregate, baseTime.Add(time.Minute)),
			event("b3", core.KindTimerStopped, core.TimerAggregate, baseTime.Add(2*time.Minute)),
		},
		{event("b4", core.KindRateSet, core.RatesAggregate, baseTime.Add(time.Hour))},
	}
	for i, batch := range batches {
		if err := s.Append(t.Context(), batch...); err != nil {
			t.Fatalf("Append batch %d: %v", i, err)
		}
	}

	got := mustLoad(t, s, 0)
	if len(got) != 4 {
		t.Fatalf("Load returned %d events, want 4", len(got))
	}
	var prev int64
	for i, e := range got {
		if e.Seq <= prev {
			t.Errorf("event %d (%s): Seq = %d, not greater than previous %d", i, e.ID, e.Seq, prev)
		}
		prev = e.Seq
	}
}

func TestAppendNoEventsIsNoOp(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	if err := s.Append(t.Context()); err != nil {
		t.Fatalf("Append with no events: %v", err)
	}
	if got := mustLoad(t, s, 0); len(got) != 0 {
		t.Fatalf("log holds %d events after an empty append, want 0", len(got))
	}

	// An empty append must not consume a seq either.
	if err := s.Append(t.Context(), event("c1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got := mustLoad(t, s, 0)
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("after empty append then one append: got %d events, first Seq %d; want 1 event at Seq 1", len(got), got[0].Seq)
	}
}

func TestLoadSince(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	seed := []core.Event{
		event("d1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime),
		event("d2", core.KindTimerStarted, core.TimerAggregate, baseTime.Add(time.Minute)),
		event("d3", core.KindTimerStopped, core.TimerAggregate, baseTime.Add(2*time.Minute)),
	}
	if err := s.Append(t.Context(), seed...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	tests := []struct {
		name     string
		sinceSeq int64
		want     []string
	}{
		{name: "zero replays the whole log", sinceSeq: 0, want: []string{"d1", "d2", "d3"}},
		{name: "exclusive lower bound", sinceSeq: 1, want: []string{"d2", "d3"}},
		{name: "tail only", sinceSeq: 2, want: []string{"d3"}},
		{name: "caught up", sinceSeq: 3, want: []string{}},
		{name: "beyond the log", sinceSeq: 99, want: []string{}},
		{name: "negative behaves like zero", sinceSeq: -1, want: []string{"d1", "d2", "d3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ids(mustLoad(t, s, tt.sinceSeq))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Load(%d) = %v, want %v", tt.sinceSeq, got, tt.want)
			}
		})
	}
}

// TestAppendFailureLeavesLogUnchanged is the atomicity test: a batch that fails
// partway must contribute nothing, not a prefix.
func TestAppendFailureLeavesLogUnchanged(t *testing.T) {
	t.Parallel()

	seed := []core.Event{
		event("e1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime),
		event("e2", core.KindTimerStarted, core.TimerAggregate, baseTime.Add(time.Minute)),
	}

	tests := []struct {
		name string
		bad  []core.Event
		want error
	}{
		{
			// The duplicate is the third of four, so an implementation without a
			// transaction would leave two extra rows behind.
			name: "duplicate of an already-stored id",
			bad: []core.Event{
				event("e3", core.KindEntryAdded, core.EntryAggregate("x"), baseTime.Add(2*time.Minute)),
				event("e4", core.KindEntryAdded, core.EntryAggregate("y"), baseTime.Add(3*time.Minute)),
				event("e1", core.KindEntryAdded, core.EntryAggregate("z"), baseTime.Add(4*time.Minute)),
				event("e5", core.KindEntryAdded, core.EntryAggregate("w"), baseTime.Add(5*time.Minute)),
			},
			want: core.ErrConflict,
		},
		{
			// Both events are new; they collide with each other inside the batch.
			name: "duplicate within the batch",
			bad: []core.Event{
				event("e6", core.KindEntryAdded, core.EntryAggregate("x"), baseTime.Add(2*time.Minute)),
				event("e6", core.KindEntryAdded, core.EntryAggregate("y"), baseTime.Add(3*time.Minute)),
			},
			want: core.ErrConflict,
		},
		{
			// Validation rejects the batch before any row is written.
			name: "invalid event late in the batch",
			bad: []core.Event{
				event("e7", core.KindEntryAdded, core.EntryAggregate("x"), baseTime.Add(2*time.Minute)),
				event("e8", "", core.EntryAggregate("y"), baseTime.Add(3*time.Minute)),
			},
			want: core.ErrInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			if err := s.Append(t.Context(), seed...); err != nil {
				t.Fatalf("seed Append: %v", err)
			}
			before := mustLoad(t, s, 0)

			err := s.Append(t.Context(), tt.bad...)
			if err == nil {
				t.Fatal("Append succeeded, want failure")
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("Append error = %v, want one matching %v", err, tt.want)
			}

			after := mustLoad(t, s, 0)
			if !reflect.DeepEqual(ids(after), ids(before)) {
				t.Fatalf("log changed after a failed append: %v, want %v", ids(after), ids(before))
			}
			for i := range after {
				if after[i].Seq != before[i].Seq {
					t.Errorf("event %d: Seq = %d after failed append, want %d", i, after[i].Seq, before[i].Seq)
				}
			}

			// The store must still be usable: a rolled-back transaction should
			// not have poisoned the connection.
			next := event("recovered", core.KindEntryAdded, core.EntryAggregate("ok"), baseTime.Add(time.Hour))
			if err := s.Append(t.Context(), next); err != nil {
				t.Fatalf("Append after a failed append: %v", err)
			}
			tail := mustLoad(t, s, before[len(before)-1].Seq)
			if len(tail) != 1 || tail[0].ID != "recovered" {
				t.Fatalf("tail after recovery = %v, want [recovered]", ids(tail))
			}
		})
	}
}

func TestAppendDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	events := []core.Event{
		event("f1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime),
		event("f2", core.KindTimerStarted, core.TimerAggregate, baseTime.Add(time.Minute)),
	}
	want := make([]core.Event, len(events))
	copy(want, events)

	if err := s.Append(t.Context(), events...); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("Append mutated the caller's events:\n got %+v\nwant %+v", events, want)
	}
	// Named explicitly because assigning Seq back is the tempting bug.
	for i, e := range events {
		if e.Seq != 0 {
			t.Errorf("event %d: caller's Seq = %d after Append, want 0", i, e.Seq)
		}
	}
}

func TestAppendRejectsInvalidEvents(t *testing.T) {
	t.Parallel()

	valid := event("g1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime)
	tests := []struct {
		name  string
		event core.Event
	}{
		{name: "empty id", event: core.Event{Kind: valid.Kind, Aggregate: valid.Aggregate, At: baseTime, Payload: valid.Payload}},
		{name: "empty kind", event: core.Event{ID: "g1", Aggregate: valid.Aggregate, At: baseTime, Payload: valid.Payload}},
		{name: "empty aggregate", event: core.Event{ID: "g1", Kind: valid.Kind, At: baseTime, Payload: valid.Payload}},
		{name: "empty payload", event: core.Event{ID: "g1", Kind: valid.Kind, Aggregate: valid.Aggregate, At: baseTime}},
	}

	s := newStore(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.Append(t.Context(), tt.event)
			if !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("Append error = %v, want one matching core.ErrInvalid", err)
			}
		})
	}
	if got := mustLoad(t, s, 0); len(got) != 0 {
		t.Fatalf("log holds %d events after only invalid appends, want 0", len(got))
	}
}

func TestOpenCreatesParentDirectories(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "deeper", "log.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	// The -wal sidecar is proof the journal_mode(WAL) pragma in the DSN took
	// effect; without it SQLite would have used a rollback journal instead.
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Errorf("no WAL sidecar for %q, so journal_mode(WAL) did not apply: %v", path, err)
	}
}

func TestOpenIsIdempotentAcrossProcesses(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "log.db")
	first, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Append(t.Context(), event("h1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Re-opening re-runs goose against an already-migrated schema; it must be a
	// no-op that leaves the existing log intact.
	second, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()

	got := mustLoad(t, second, 0)
	if len(got) != 1 || got[0].ID != "h1" || got[0].Seq != 1 {
		t.Fatalf("reopened log = %v, want [h1] at Seq 1", ids(got))
	}
	if err := second.Append(t.Context(), event("h2", core.KindProjectArchived, core.ProjectAggregate("acme"), baseTime.Add(time.Hour))); err != nil {
		t.Fatalf("Append after reopen: %v", err)
	}
	if got := mustLoad(t, second, 1); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("after reopen and append, Load(1) = %v, want one event at Seq 2", ids(got))
	}
}

func TestOpenInMemory(t *testing.T) {
	t.Parallel()

	s, err := Open(t.Context(), memoryPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", memoryPath, err)
	}
	defer s.Close()

	if err := s.Append(t.Context(), event("i1", core.KindProjectCreated, core.ProjectAggregate("acme"), baseTime)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := mustLoad(t, s, 0); len(got) != 1 || got[0].ID != "i1" {
		t.Fatalf("in-memory log = %v, want [i1]", ids(got))
	}
	// The literal ":memory:" must never be treated as a filesystem path.
	if _, err := os.Stat(memoryPath); !os.IsNotExist(err) {
		t.Errorf("Open created a %q entry on disk (stat err = %v)", memoryPath, err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	s, err := Open(t.Context(), "")
	if err == nil {
		s.Close()
		t.Fatal("Open(\"\") succeeded, want failure")
	}
	if !errors.Is(err, core.ErrInvalid) {
		t.Errorf("Open(\"\") error = %v, want one matching core.ErrInvalid", err)
	}
}

// TestLoadCancelledContext pins that Load honours the caller's context rather
// than running to completion on a cancelled request.
func TestLoadCancelledContext(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Load(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Load with cancelled context: err = %v, want context.Canceled", err)
	}
}
