package timer

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// t0 is the instant every test starts from. It is UTC because the log is
// canonical storage and the Service normalises to UTC.
var t0 = time.Date(2026, time.March, 3, 9, 0, 0, 0, time.UTC)

// fakeLog is an in-memory core.Log: a slice of events with a store-assigned
// Seq. appendErr makes Append fail so the persist-then-publish ordering can be
// tested; a failing Append records nothing, like a rolled-back transaction.
type fakeLog struct {
	events    []core.Event
	seq       int64
	appendErr error
}

func (l *fakeLog) Append(_ context.Context, events ...core.Event) error {
	if l.appendErr != nil {
		return l.appendErr
	}
	for _, e := range events {
		l.seq++
		e.Seq = l.seq
		l.events = append(l.events, e)
	}
	return nil
}

func (l *fakeLog) Load(_ context.Context, sinceSeq int64) ([]core.Event, error) {
	var out []core.Event
	for _, e := range l.events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// publication is one recorded Publisher call.
type publication struct {
	subject     string
	aggregateID string
}

// fakePub records every Publish so a test can assert on both what was
// announced and, critically, that nothing was announced at all.
type fakePub struct{ calls []publication }

func (p *fakePub) Publish(subject, aggregateID string) {
	p.calls = append(p.calls, publication{subject: subject, aggregateID: aggregateID})
}

// newAt builds a Service over log whose clock is frozen at at, so a test can
// advance time simply by building a second Service over the same log.
func newAt(log core.Log, pub core.Publisher, at time.Time) *Service {
	return NewService(log, pub, core.FixedClock(at))
}

// timeline replays everything the fake log holds, which is how a test asserts
// on what a read model would actually see.
func timeline(t *testing.T, log *fakeLog, now time.Time) core.Timeline {
	t.Helper()
	events, err := log.Load(context.Background(), 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tl, err := core.BuildTimeline(events, now)
	if err != nil {
		t.Fatalf("build timeline: %v", err)
	}
	return tl
}

func TestStartThenRunning(t *testing.T) {
	ctx := context.Background()
	log, pub := &fakeLog{}, &fakePub{}

	started, err := newAt(log, pub, t0).Start(ctx, "tempo", "wiring the write model", []string{"go", "cqrs"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !started.Running {
		t.Error("started view: Running = false, want true")
	}
	if !started.Start.Equal(t0) {
		t.Errorf("started view: Start = %v, want %v", started.Start, t0)
	}
	if started.Duration != 0 {
		t.Errorf("started view: Duration = %v, want 0", started.Duration)
	}
	if len(log.events) != 1 || log.events[0].Kind != core.KindTimerStarted {
		t.Fatalf("log = %+v, want exactly one %s", log.events, core.KindTimerStarted)
	}
	if got, want := log.events[0].Aggregate, core.EntryAggregate(started.ID); got != want {
		t.Errorf("aggregate = %q, want %q", got, want)
	}
	if len(pub.calls) != 1 || pub.calls[0] != (publication{core.SubjectTimer, started.ID}) {
		t.Errorf("publications = %+v, want one on %s for %s", pub.calls, core.SubjectTimer, started.ID)
	}

	// Ninety minutes later the same log reports the same entry, still open,
	// with its duration measured to the reading clock.
	running, ok, err := newAt(log, pub, t0.Add(90*time.Minute)).Running(ctx)
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if !ok {
		t.Fatal("Running: ok = false, want true")
	}
	if running.ID != started.ID {
		t.Errorf("Running: ID = %q, want %q", running.ID, started.ID)
	}
	if running.Project != "tempo" || running.Note != "wiring the write model" {
		t.Errorf("Running: project/note = %q/%q, want %q/%q", running.Project, running.Note, "tempo", "wiring the write model")
	}
	if want := []string{"cqrs", "go"}; !slices.Equal(running.Tags, want) {
		t.Errorf("Running: Tags = %v, want %v", running.Tags, want)
	}
	if running.Duration != 90*time.Minute {
		t.Errorf("Running: Duration = %v, want %v", running.Duration, 90*time.Minute)
	}
	if !running.End.IsZero() {
		t.Errorf("Running: End = %v, want zero while running", running.End)
	}
}

func TestRunningWhenIdle(t *testing.T) {
	view, ok, err := newAt(&fakeLog{}, &fakePub{}, t0).Running(context.Background())
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if ok {
		t.Error("Running: ok = true on an empty log, want false")
	}
	if !reflect.DeepEqual(view, core.EntryView{}) {
		t.Errorf("Running: view = %+v, want zero value", view)
	}
}

// TestStartTwiceConflicts pins the headline invariant: at most one timer runs,
// and the refused command leaves the log untouched.
func TestStartTwiceConflicts(t *testing.T) {
	ctx := context.Background()
	log, pub := &fakeLog{}, &fakePub{}

	if _, err := newAt(log, pub, t0).Start(ctx, "tempo", "", nil); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	_, err := newAt(log, pub, t0.Add(time.Minute)).Start(ctx, "other", "", nil)
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("second Start: err = %v, want one wrapping core.ErrConflict", err)
	}
	if len(log.events) != 1 {
		t.Errorf("log holds %d events, want 1: a refused Start must append nothing", len(log.events))
	}
	if len(pub.calls) != 1 {
		t.Errorf("publications = %+v, want only the first Start's", pub.calls)
	}
}

func TestStartInvalidProject(t *testing.T) {
	tests := []struct {
		name    string
		project string
	}{
		{name: "empty", project: ""},
		{name: "blank", project: "   "},
		{name: "tabs and newline", project: "\t\n "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, pub := &fakeLog{}, &fakePub{}
			_, err := newAt(log, pub, t0).Start(context.Background(), tt.project, "note", nil)
			if !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("Start(%q): err = %v, want one wrapping core.ErrInvalid", tt.project, err)
			}
			if len(log.events) != 0 || len(pub.calls) != 0 {
				t.Errorf("rejected Start wrote %d events and %d publications, want 0 and 0", len(log.events), len(pub.calls))
			}
		})
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	log, pub := &fakeLog{}, &fakePub{}
	_, err := newAt(log, pub, t0).Stop(context.Background())
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Stop: err = %v, want one wrapping core.ErrNotFound", err)
	}
	if len(log.events) != 0 || len(pub.calls) != 0 {
		t.Errorf("failed Stop wrote %d events and %d publications, want 0 and 0", len(log.events), len(pub.calls))
	}
}

func TestCancelWithNothingRunning(t *testing.T) {
	log, pub := &fakeLog{}, &fakePub{}
	err := newAt(log, pub, t0).Cancel(context.Background())
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Cancel: err = %v, want one wrapping core.ErrNotFound", err)
	}
	if len(log.events) != 0 || len(pub.calls) != 0 {
		t.Errorf("failed Cancel wrote %d events and %d publications, want 0 and 0", len(log.events), len(pub.calls))
	}
}

func TestStartStopRoundTrip(t *testing.T) {
	ctx := context.Background()
	log, pub := &fakeLog{}, &fakePub{}
	const worked = 2*time.Hour + 15*time.Minute
	stoppedAt := t0.Add(worked)

	started, err := newAt(log, pub, t0).Start(ctx, "tempo", "round trip", []string{"go"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	done, err := newAt(log, pub, stoppedAt).Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if done.ID != started.ID {
		t.Errorf("Stop: ID = %q, want %q", done.ID, started.ID)
	}
	if done.Running {
		t.Error("Stop: Running = true, want false")
	}
	if !done.End.Equal(stoppedAt) {
		t.Errorf("Stop: End = %v, want %v", done.End, stoppedAt)
	}
	if done.Duration != worked {
		t.Errorf("Stop: Duration = %v, want %v", done.Duration, worked)
	}
	if done.Project != "tempo" || done.Note != "round trip" {
		t.Errorf("Stop: project/note = %q/%q, want %q/%q", done.Project, done.Note, "tempo", "round trip")
	}

	if len(log.events) != 2 || log.events[1].Kind != core.KindTimerStopped {
		t.Fatalf("log = %+v, want a started then a stopped event", log.events)
	}
	if got, want := log.events[1].Aggregate, core.EntryAggregate(started.ID); got != want {
		t.Errorf("stop aggregate = %q, want %q", got, want)
	}
	if len(pub.calls) != 2 || pub.calls[1] != (publication{core.SubjectTimer, started.ID}) {
		t.Errorf("publications = %+v, want a second one on %s for %s", pub.calls, core.SubjectTimer, started.ID)
	}

	// A rebuilt timeline must agree with what Stop returned; the write model
	// is not allowed a private opinion about the entry it just closed.
	tl := timeline(t, log, stoppedAt.Add(time.Hour))
	if tl.Running != nil {
		t.Errorf("timeline still running: %+v", tl.Running)
	}
	if len(tl.Entries) != 1 || tl.Entries[0].Duration != worked {
		t.Fatalf("timeline entries = %+v, want one of %v", tl.Entries, worked)
	}

	// Stopping again is ErrNotFound: the interval is closed, not re-closable.
	if _, err := newAt(log, pub, stoppedAt.Add(time.Minute)).Stop(ctx); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("second Stop: err = %v, want one wrapping core.ErrNotFound", err)
	}
}

// TestCancelErasesEntry checks that a cancelled timer contributes zero time:
// replay must show no trace of it, not a zero-length entry.
func TestCancelErasesEntry(t *testing.T) {
	ctx := context.Background()
	log, pub := &fakeLog{}, &fakePub{}

	started, err := newAt(log, pub, t0).Start(ctx, "tempo", "mistake", []string{"go"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := newAt(log, pub, t0.Add(10*time.Minute)).Cancel(ctx); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if len(log.events) != 2 || log.events[1].Kind != core.KindEntryDeleted {
		t.Fatalf("log = %+v, want a started then an %s event", log.events, core.KindEntryDeleted)
	}
	if got, want := log.events[1].Aggregate, core.EntryAggregate(started.ID); got != want {
		t.Errorf("cancel aggregate = %q, want %q", got, want)
	}
	if len(pub.calls) != 2 || pub.calls[1] != (publication{core.SubjectTimer, started.ID}) {
		t.Errorf("publications = %+v, want a second one on %s for %s", pub.calls, core.SubjectTimer, started.ID)
	}

	tl := timeline(t, log, t0.Add(time.Hour))
	if tl.Running != nil {
		t.Errorf("timeline still running after cancel: %+v", tl.Running)
	}
	if len(tl.Entries) != 0 {
		t.Errorf("timeline entries = %+v, want none: a cancelled timer records no time", tl.Entries)
	}

	// The slot is free again, which is the point of cancelling.
	if _, err := newAt(log, pub, t0.Add(time.Hour)).Start(ctx, "tempo", "for real", nil); err != nil {
		t.Errorf("Start after Cancel: %v", err)
	}
}

// TestAppendErrorPublishesNothing is the critical ordering test: persist, then
// publish. A write that did not land must never be announced.
func TestAppendErrorPublishesNothing(t *testing.T) {
	ctx := context.Background()
	wantErr := errors.New("disk on fire")

	tests := []struct {
		name string
		// seed puts the log into the state the command needs; it runs while
		// Append still works.
		seed func(t *testing.T, log *fakeLog)
		// run issues the command that must fail.
		run func(s *Service) error
	}{
		{
			name: "start",
			seed: func(*testing.T, *fakeLog) {},
			run: func(s *Service) error {
				_, err := s.Start(ctx, "tempo", "note", []string{"go"})
				return err
			},
		},
		{
			name: "stop",
			seed: func(t *testing.T, log *fakeLog) {
				if _, err := newAt(log, nil, t0).Start(ctx, "tempo", "note", nil); err != nil {
					t.Fatalf("seed Start: %v", err)
				}
			},
			run: func(s *Service) error {
				_, err := s.Stop(ctx)
				return err
			},
		},
		{
			name: "cancel",
			seed: func(t *testing.T, log *fakeLog) {
				if _, err := newAt(log, nil, t0).Start(ctx, "tempo", "note", nil); err != nil {
					t.Fatalf("seed Start: %v", err)
				}
			},
			run: func(s *Service) error { return s.Cancel(ctx) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &fakeLog{}
			tt.seed(t, log)
			seeded := len(log.events)

			// The publisher is attached only now, so it can only ever have
			// recorded calls made by the failing command.
			pub := &fakePub{}
			log.appendErr = wantErr

			err := tt.run(newAt(log, pub, t0.Add(time.Hour)))
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want one wrapping %v", err, wantErr)
			}
			if len(pub.calls) != 0 {
				t.Errorf("publications = %+v, want none: a failed append must publish nothing", pub.calls)
			}
			if len(log.events) != seeded {
				t.Errorf("log holds %d events, want %d unchanged", len(log.events), seeded)
			}
		})
	}
}

// TestNilPublisher covers the CLI configuration: no bus, and that is fine.
func TestNilPublisher(t *testing.T) {
	ctx := context.Background()
	log := &fakeLog{}

	if _, err := newAt(log, nil, t0).Start(ctx, "tempo", "", nil); err != nil {
		t.Fatalf("Start with nil publisher: %v", err)
	}
	if _, err := newAt(log, nil, t0.Add(time.Hour)).Stop(ctx); err != nil {
		t.Fatalf("Stop with nil publisher: %v", err)
	}
	if _, err := newAt(log, nil, t0.Add(2*time.Hour)).Start(ctx, "tempo", "", nil); err != nil {
		t.Fatalf("second Start with nil publisher: %v", err)
	}
	if err := newAt(log, nil, t0.Add(3*time.Hour)).Cancel(ctx); err != nil {
		t.Fatalf("Cancel with nil publisher: %v", err)
	}
}

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil stays nil", in: nil, want: nil},
		{name: "empty slice becomes nil", in: []string{}, want: nil},
		{name: "all blank becomes nil", in: []string{"", "  ", "\t"}, want: nil},
		{name: "trims", in: []string{"  go  "}, want: []string{"go"}},
		{name: "lowercases", in: []string{"Go", "CQRS"}, want: []string{"cqrs", "go"}},
		{name: "sorts", in: []string{"zeta", "alpha", "mid"}, want: []string{"alpha", "mid", "zeta"}},
		{name: "drops empties", in: []string{"go", "", "  ", "cqrs"}, want: []string{"cqrs", "go"}},
		{
			name: "de-duplicates across spellings",
			in:   []string{"Go", " go ", "GO", "go"},
			want: []string{"go"},
		},
		{
			name: "everything at once",
			in:   []string{" Refactor", "go", "", "GO", "  ", "billing "},
			want: []string{"billing", "go", "refactor"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTags(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("normalizeTags(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// nil and an empty non-nil slice are equal to slices.Equal but
			// encode differently, and the contract is that empty round-trips
			// as nil.
			if tt.want == nil && got != nil {
				t.Errorf("normalizeTags(%q) = %#v, want nil not an empty slice", tt.in, got)
			}
		})
	}
}

// TestStartNormalisesTagsInLog checks the normalisation actually reaches the
// event, not just the returned view — the log is what every read model folds.
func TestStartNormalisesTagsInLog(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "messy set", in: []string{" Refactor", "go", "GO", ""}, want: []string{"go", "refactor"}},
		{name: "no tags", in: nil, want: nil},
		{name: "only blanks", in: []string{" ", ""}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			log := &fakeLog{}

			view, err := newAt(log, &fakePub{}, t0).Start(ctx, "tempo", "", tt.in)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !slices.Equal(view.Tags, tt.want) {
				t.Errorf("view.Tags = %q, want %q", view.Tags, tt.want)
			}
			if tt.want == nil && view.Tags != nil {
				t.Errorf("view.Tags = %#v, want nil", view.Tags)
			}

			payload, err := core.DecodePayload[core.TimerStarted](log.events[0])
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !slices.Equal(payload.Tags, tt.want) {
				t.Errorf("payload.Tags = %q, want %q", payload.Tags, tt.want)
			}
			if tt.want == nil && payload.Tags != nil {
				t.Errorf("payload.Tags = %#v, want nil", payload.Tags)
			}
			if !payload.StartedAt.Equal(t0) {
				t.Errorf("payload.StartedAt = %v, want %v", payload.StartedAt, t0)
			}
			if payload.EntryID != view.ID {
				t.Errorf("payload.EntryID = %q, want %q", payload.EntryID, view.ID)
			}
		})
	}
}
