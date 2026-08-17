package worklog

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// testNow is the instant every test's clock reports, so event timestamps and
// running-entry durations are deterministic.
var testNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

// fakeLog is an in-memory core.Log. Append assigns Seq the way a real store
// does, which is the only store behaviour this package depends on.
type fakeLog struct {
	mu        sync.Mutex
	events    []core.Event
	appendErr error // when set, Append fails without recording anything
	loadErr   error
}

func (l *fakeLog) Append(_ context.Context, events ...core.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.appendErr != nil {
		return l.appendErr
	}
	for _, e := range events {
		e.Seq = int64(len(l.events) + 1)
		l.events = append(l.events, e)
	}
	return nil
}

func (l *fakeLog) Load(_ context.Context, sinceSeq int64) ([]core.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadErr != nil {
		return nil, l.loadErr
	}
	out := make([]core.Event, 0, len(l.events))
	for _, e := range l.events {
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l *fakeLog) all() []core.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func (l *fakeLog) last(t *testing.T) core.Event {
	t.Helper()
	events := l.all()
	if len(events) == 0 {
		t.Fatal("log is empty, expected at least one event")
	}
	return events[len(events)-1]
}

// pubCall records one Publisher invocation.
type pubCall struct {
	subject     string
	aggregateID string
}

// fakePub records every publish so a test can assert both what was announced
// and, crucially, that nothing was announced on a failed write.
type fakePub struct {
	mu    sync.Mutex
	calls []pubCall
}

func (p *fakePub) Publish(subject, aggregateID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, pubCall{subject, aggregateID})
}

func (p *fakePub) snapshot() []pubCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func newTestService(t *testing.T) (*Service, *fakeLog, *fakePub) {
	t.Helper()
	lg := &fakeLog{}
	pub := &fakePub{}
	return NewService(lg, pub, core.FixedClock(testNow)), lg, pub
}

// seedEntry adds one complete entry and returns its id.
func seedEntry(t *testing.T, s *Service, project string, start, end time.Time) string {
	t.Helper()
	view, err := s.Add(context.Background(), project, "seed", nil, start, end)
	if err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	return view.ID
}

// seedRunning writes a timer.started event directly, since only internal/timer
// may open an interval.
func seedRunning(t *testing.T, lg *fakeLog, id string, start time.Time) {
	t.Helper()
	ev, err := core.NewEvent(core.KindTimerStarted, core.TimerAggregate, start, core.TimerStarted{
		EntryID:   id,
		Project:   "acme",
		Note:      "running",
		StartedAt: start.UTC(),
	})
	if err != nil {
		t.Fatalf("seed NewEvent: %v", err)
	}
	if err := lg.Append(context.Background(), ev); err != nil {
		t.Fatalf("seed Append: %v", err)
	}
}

func TestAddHappyPath(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)

	view, err := s.Add(ctx, "  acme  ", "wrote the fold", []string{"Deep Work"}, start, end)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if view.ID == "" {
		t.Fatal("Add returned an empty ID")
	}
	if view.Project != "acme" {
		t.Errorf("Project = %q, want %q", view.Project, "acme")
	}
	if view.Note != "wrote the fold" {
		t.Errorf("Note = %q, want %q", view.Note, "wrote the fold")
	}
	if view.Duration != 90*time.Minute {
		t.Errorf("Duration = %v, want %v", view.Duration, 90*time.Minute)
	}
	if view.Running {
		t.Error("Running = true, want false")
	}
	if !view.Start.Equal(start) || !view.End.Equal(end) {
		t.Errorf("interval = [%v, %v), want [%v, %v)", view.Start, view.End, start, end)
	}
	if view.Start.Location() != time.UTC || view.End.Location() != time.UTC {
		t.Errorf("times not stored in UTC: %v, %v", view.Start.Location(), view.End.Location())
	}

	events := lg.all()
	if len(events) != 1 {
		t.Fatalf("appended %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Kind != core.KindEntryAdded {
		t.Errorf("Kind = %q, want %q", ev.Kind, core.KindEntryAdded)
	}
	if want := core.EntryAggregate(view.ID); ev.Aggregate != want {
		t.Errorf("Aggregate = %q, want %q", ev.Aggregate, want)
	}
	if !ev.At.Equal(testNow) {
		t.Errorf("At = %v, want %v", ev.At, testNow)
	}
	payload, err := core.DecodePayload[core.EntryAdded](ev)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.EntryID != view.ID || payload.Project != "acme" || payload.Note != "wrote the fold" {
		t.Errorf("payload = %+v, want id %q project %q", payload, view.ID, "acme")
	}
	if !slices.Equal(payload.Tags, []string{"deep work"}) {
		t.Errorf("payload.Tags = %v, want [deep work]", payload.Tags)
	}

	calls := pub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("published %d times, want 1", len(calls))
	}
	if calls[0] != (pubCall{core.SubjectEntries, view.ID}) {
		t.Errorf("published %+v, want {%q %q}", calls[0], core.SubjectEntries, view.ID)
	}
}

func TestAddInvalid(t *testing.T) {
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		project      string
		start, end   time.Time
		wantSentinel error
	}{
		{"empty project", "", start, start.Add(time.Hour), core.ErrInvalid},
		{"whitespace project", "   \t ", start, start.Add(time.Hour), core.ErrInvalid},
		{"end equals start", "acme", start, start, core.ErrInvalid},
		{"end before start", "acme", start, start.Add(-time.Second), core.ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, lg, pub := newTestService(t)

			_, err := s.Add(ctx, tt.project, "note", nil, tt.start, tt.end)
			if !errors.Is(err, tt.wantSentinel) {
				t.Fatalf("Add error = %v, want %v", err, tt.wantSentinel)
			}
			if got := len(lg.all()); got != 0 {
				t.Errorf("appended %d events, want 0", got)
			}
			if got := len(pub.snapshot()); got != 0 {
				t.Errorf("published %d times, want 0", got)
			}
		})
	}
}

func TestAddNormalizesTags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil stays nil", nil, nil},
		{"empty stays nil", []string{}, nil},
		{"only blanks becomes nil", []string{"  ", "\t", ""}, nil},
		{"trimmed lowercased sorted", []string{" Zeta ", "Alpha"}, []string{"alpha", "zeta"}},
		{"de-duplicated case-insensitively", []string{"Focus", "focus", " FOCUS "}, []string{"focus"}},
		{"blanks dropped from a real set", []string{"b", "", "  ", "a"}, []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, lg, _ := newTestService(t)

			start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
			view, err := s.Add(ctx, "acme", "note", tt.in, start, start.Add(time.Hour))
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if !slices.Equal(view.Tags, tt.want) {
				t.Errorf("view.Tags = %#v, want %#v", view.Tags, tt.want)
			}
			if tt.want == nil && view.Tags != nil {
				t.Errorf("view.Tags = %#v, want nil", view.Tags)
			}
			payload, err := core.DecodePayload[core.EntryAdded](lg.last(t))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !slices.Equal(payload.Tags, tt.want) {
				t.Errorf("payload.Tags = %#v, want %#v", payload.Tags, tt.want)
			}
		})
	}
}

// TestAddAppendErrorPublishesNothing is the load-bearing test of this package:
// a write that did not persist must never be announced.
func TestAddAppendErrorPublishesNothing(t *testing.T) {
	ctx := context.Background()
	lg := &fakeLog{appendErr: errors.New("disk on fire")}
	pub := &fakePub{}
	s := NewService(lg, pub, core.FixedClock(testNow))

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	if _, err := s.Add(ctx, "acme", "note", nil, start, start.Add(time.Hour)); err == nil {
		t.Fatal("Add succeeded, want append error")
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Fatalf("published %d times after a failed append, want 0", got)
	}
	if got := len(lg.all()); got != 0 {
		t.Fatalf("recorded %d events after a failed append, want 0", got)
	}
}

func TestEditAppendErrorPublishesNothing(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	id := seedEntry(t, s, "acme", start, start.Add(time.Hour))
	before := len(lg.all())
	pub.mu.Lock()
	pub.calls = nil
	pub.mu.Unlock()

	lg.appendErr = errors.New("disk on fire")
	note := "nope"
	if _, err := s.Edit(ctx, id, Patch{Note: &note}); err == nil {
		t.Fatal("Edit succeeded, want append error")
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Fatalf("published %d times after a failed append, want 0", got)
	}
	if got := len(lg.all()); got != before {
		t.Fatalf("log grew to %d events after a failed append, want %d", got, before)
	}
}

func TestDeleteAppendErrorPublishesNothing(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	id := seedEntry(t, s, "acme", start, start.Add(time.Hour))
	before := len(lg.all())
	pub.mu.Lock()
	pub.calls = nil
	pub.mu.Unlock()

	lg.appendErr = errors.New("disk on fire")
	if err := s.Delete(ctx, id); err == nil {
		t.Fatal("Delete succeeded, want append error")
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Fatalf("published %d times after a failed append, want 0", got)
	}
	if got := len(lg.all()); got != before {
		t.Fatalf("log grew to %d events after a failed append, want %d", got, before)
	}
}

func TestEditUnknownEntry(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	note := "hello"
	if _, err := s.Edit(ctx, "nosuchid", Patch{Note: &note}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Edit error = %v, want %v", err, core.ErrNotFound)
	}
	if got := len(lg.all()); got != 0 {
		t.Errorf("appended %d events, want 0", got)
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Errorf("published %d times, want 0", got)
	}
}

func TestDeleteThenEditIsNotFound(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	id := seedEntry(t, s, "acme", start, start.Add(time.Hour))

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	afterDelete := len(lg.all())
	publishesAfterDelete := len(pub.snapshot())

	note := "resurrect"
	if _, err := s.Edit(ctx, id, Patch{Note: &note}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Edit after Delete error = %v, want %v", err, core.ErrNotFound)
	}
	if got := len(lg.all()); got != afterDelete {
		t.Errorf("log grew to %d events, want %d", got, afterDelete)
	}
	if got := len(pub.snapshot()); got != publishesAfterDelete {
		t.Errorf("published %d times, want %d", got, publishesAfterDelete)
	}
	if err := s.Delete(ctx, id); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("second Delete error = %v, want %v", err, core.ErrNotFound)
	}
}

func TestDeleteHappyPath(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	id := seedEntry(t, s, "acme", start, start.Add(time.Hour))

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ev := lg.last(t)
	if ev.Kind != core.KindEntryDeleted {
		t.Errorf("Kind = %q, want %q", ev.Kind, core.KindEntryDeleted)
	}
	if want := core.EntryAggregate(id); ev.Aggregate != want {
		t.Errorf("Aggregate = %q, want %q", ev.Aggregate, want)
	}
	payload, err := core.DecodePayload[core.EntryDeleted](ev)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.EntryID != id {
		t.Errorf("payload.EntryID = %q, want %q", payload.EntryID, id)
	}

	calls := pub.snapshot()
	if len(calls) != 2 {
		t.Fatalf("published %d times, want 2 (add + delete)", len(calls))
	}
	if calls[1] != (pubCall{core.SubjectEntries, id}) {
		t.Errorf("published %+v, want {%q %q}", calls[1], core.SubjectEntries, id)
	}

	got, err := s.List(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d entries after delete, want 0", len(got))
	}
}

func TestEditInvertedIntervalAppendsNothing(t *testing.T) {
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	movedStart := end.Add(time.Minute)  // start after the untouched end
	movedEnd := start.Add(-time.Minute) // end before the untouched start
	equalEnd := start                   // end exactly at start is not strictly after

	tests := []struct {
		name  string
		patch Patch
	}{
		{"start moved past end", Patch{Start: &movedStart}},
		{"end moved before start", Patch{End: &movedEnd}},
		{"end equal to start", Patch{End: &equalEnd}},
		{"both moved and inverted", Patch{Start: &end, End: &start}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, lg, pub := newTestService(t)
			id := seedEntry(t, s, "acme", start, end)
			before := len(lg.all())
			publishesBefore := len(pub.snapshot())

			if _, err := s.Edit(ctx, id, tt.patch); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("Edit error = %v, want %v", err, core.ErrInvalid)
			}
			if got := len(lg.all()); got != before {
				t.Errorf("log grew to %d events, want %d", got, before)
			}
			if got := len(pub.snapshot()); got != publishesBefore {
				t.Errorf("published %d times, want %d", got, publishesBefore)
			}
		})
	}
}

func TestEditEmptyPatchIsANoOp(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	id := seedEntry(t, s, "acme", start, end)
	before := len(lg.all())
	publishesBefore := len(pub.snapshot())

	view, err := s.Edit(ctx, id, Patch{})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if view.ID != id || view.Project != "acme" || view.Note != "seed" {
		t.Errorf("view = %+v, want the unchanged entry", view)
	}
	if !view.Start.Equal(start) || !view.End.Equal(end) || view.Duration != time.Hour {
		t.Errorf("view interval = [%v, %v) %v, want [%v, %v) %v", view.Start, view.End, view.Duration, start, end, time.Hour)
	}
	if got := len(lg.all()); got != before {
		t.Errorf("log grew to %d events, want %d", got, before)
	}
	if got := len(pub.snapshot()); got != publishesBefore {
		t.Errorf("published %d times, want %d", got, publishesBefore)
	}
}

func TestEditWritesOnlyTheFieldsSet(t *testing.T) {
	ctx := context.Background()
	s, lg, pub := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	id := seedEntry(t, s, "acme", start, end)
	publishesBefore := len(pub.snapshot())

	// A pointer to "" clears the note; every other field must stay absent from
	// the recorded patch so replay leaves it alone.
	cleared := ""
	view, err := s.Edit(ctx, id, Patch{Note: &cleared})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	payload, err := core.DecodePayload[core.EntryEdited](lg.last(t))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.EntryID != id {
		t.Errorf("payload.EntryID = %q, want %q", payload.EntryID, id)
	}
	if payload.Note == nil {
		t.Fatal("payload.Note = nil, want a pointer to the empty string")
	}
	if *payload.Note != "" {
		t.Errorf("*payload.Note = %q, want %q", *payload.Note, "")
	}
	if payload.Project != nil {
		t.Errorf("payload.Project = %q, want nil", *payload.Project)
	}
	if payload.Tags != nil {
		t.Errorf("payload.Tags = %v, want nil", *payload.Tags)
	}
	if payload.Start != nil {
		t.Errorf("payload.Start = %v, want nil", *payload.Start)
	}
	if payload.End != nil {
		t.Errorf("payload.End = %v, want nil", *payload.End)
	}

	if view.Note != "" {
		t.Errorf("view.Note = %q, want %q", view.Note, "")
	}
	if view.Project != "acme" || !view.Start.Equal(start) || !view.End.Equal(end) {
		t.Errorf("view = %+v, want everything but Note unchanged", view)
	}
	if view.Duration != time.Hour {
		t.Errorf("view.Duration = %v, want %v", view.Duration, time.Hour)
	}

	calls := pub.snapshot()
	if len(calls) != publishesBefore+1 {
		t.Fatalf("published %d times, want %d", len(calls), publishesBefore+1)
	}
	if last := calls[len(calls)-1]; last != (pubCall{core.SubjectEntries, id}) {
		t.Errorf("published %+v, want {%q %q}", last, core.SubjectEntries, id)
	}
}

func TestEditNormalizesTags(t *testing.T) {
	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	tests := []struct {
		name string
		in   []string
		want []string // the tags the payload must carry, non-nil in every case
	}{
		{"trimmed lowercased sorted deduped", []string{" Zeta", "alpha", "ALPHA "}, []string{"alpha", "zeta"}},
		{"cleared with an empty slice", []string{}, []string{}},
		{"cleared with only blanks", []string{" ", ""}, []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, lg, _ := newTestService(t)
			id := seedEntry(t, s, "acme", start, end)

			in := slices.Clone(tt.in)
			view, err := s.Edit(ctx, id, Patch{Tags: &in})
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}

			payload, err := core.DecodePayload[core.EntryEdited](lg.last(t))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			// Non-nil even when empty: "clear the tags" has to survive the JSON
			// round-trip, and a null would replay as "leave alone".
			if payload.Tags == nil {
				t.Fatal("payload.Tags = nil, want a non-nil pointer")
			}
			if !slices.Equal(*payload.Tags, tt.want) {
				t.Errorf("*payload.Tags = %#v, want %#v", *payload.Tags, tt.want)
			}
			if !slices.Equal(view.Tags, tt.want) {
				t.Errorf("view.Tags = %#v, want %#v", view.Tags, tt.want)
			}
			if !slices.Equal(in, tt.in) {
				t.Errorf("Edit mutated the caller's slice: %#v, want %#v", in, tt.in)
			}
		})
	}
}

func TestEditRunningEntry(t *testing.T) {
	start := testNow.Add(-30 * time.Minute)
	moved := start.Add(-time.Hour)
	stop := testNow

	tests := []struct {
		name         string
		patch        Patch
		wantSentinel error // nil means the edit must succeed
	}{
		{"note only is allowed", Patch{Note: ptr("still going")}, nil},
		{"project and tags are allowed", Patch{Project: ptr("beta"), Tags: &[]string{"Focus"}}, nil},
		{"moving start conflicts", Patch{Start: &moved}, core.ErrConflict},
		{"setting end conflicts", Patch{End: &stop}, core.ErrConflict},
		{"note plus end still conflicts", Patch{Note: ptr("done"), End: &stop}, core.ErrConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s, lg, pub := newTestService(t)
			seedRunning(t, lg, "run-1", start)
			before := len(lg.all())

			view, err := s.Edit(ctx, "run-1", tt.patch)
			if tt.wantSentinel != nil {
				if !errors.Is(err, tt.wantSentinel) {
					t.Fatalf("Edit error = %v, want %v", err, tt.wantSentinel)
				}
				if got := len(lg.all()); got != before {
					t.Errorf("log grew to %d events, want %d", got, before)
				}
				if got := len(pub.snapshot()); got != 0 {
					t.Errorf("published %d times, want 0", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Edit: %v", err)
			}
			if !view.Running {
				t.Error("view.Running = false, want true")
			}
			if !view.End.IsZero() {
				t.Errorf("view.End = %v, want the zero time while running", view.End)
			}
			if view.Duration != 30*time.Minute {
				t.Errorf("view.Duration = %v, want %v", view.Duration, 30*time.Minute)
			}
			if got := len(lg.all()); got != before+1 {
				t.Errorf("appended %d events, want %d", got, before+1)
			}
			if got := len(pub.snapshot()); got != 1 {
				t.Errorf("published %d times, want 1", got)
			}
		})
	}
}

func TestEditProject(t *testing.T) {
	ctx := context.Background()
	s, lg, _ := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	id := seedEntry(t, s, "acme", start, start.Add(time.Hour))

	view, err := s.Edit(ctx, id, Patch{Project: ptr("  beta  ")})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if view.Project != "beta" {
		t.Errorf("view.Project = %q, want %q", view.Project, "beta")
	}
	payload, err := core.DecodePayload[core.EntryEdited](lg.last(t))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Project == nil || *payload.Project != "beta" {
		t.Errorf("payload.Project = %v, want a pointer to %q", payload.Project, "beta")
	}

	before := len(lg.all())
	if _, err := s.Edit(ctx, id, Patch{Project: ptr("   ")}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("Edit with a blank project = %v, want %v", err, core.ErrInvalid)
	}
	if got := len(lg.all()); got != before {
		t.Errorf("log grew to %d events, want %d", got, before)
	}
}

func TestEditMovesIntervalAndRecomputesDuration(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestService(t)

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	id := seedEntry(t, s, "acme", start, start.Add(time.Hour))

	newEnd := start.Add(2 * time.Hour)
	view, err := s.Edit(ctx, id, Patch{End: &newEnd})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !view.End.Equal(newEnd) {
		t.Errorf("view.End = %v, want %v", view.End, newEnd)
	}
	if view.Duration != 2*time.Hour {
		t.Errorf("view.Duration = %v, want %v", view.Duration, 2*time.Hour)
	}
	if view.End.Location() != time.UTC {
		t.Errorf("view.End location = %v, want UTC", view.End.Location())
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestService(t)

	day := func(hour int) time.Time {
		return time.Date(2026, 8, 17, hour, 0, 0, 0, time.UTC)
	}
	nine := seedEntry(t, s, "acme", day(9), day(10))
	ten := seedEntry(t, s, "beta", day(10), day(11))
	eleven := seedEntry(t, s, "gamma", day(11), day(12))

	tests := []struct {
		name     string
		from, to time.Time
		want     []string
	}{
		{"unbounded", time.Time{}, time.Time{}, []string{nine, ten, eleven}},
		{"from is inclusive", day(10), time.Time{}, []string{ten, eleven}},
		{"to is exclusive", time.Time{}, day(11), []string{nine, ten}},
		{"half-open window", day(10), day(11), []string{ten}},
		{"no lower bound only", time.Time{}, day(10), []string{nine}},
		{"empty window", day(12), day(13), nil},
		{"window before everything", day(1), day(2), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.List(ctx, tt.from, tt.to)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if got == nil {
				t.Fatal("List returned nil, want a non-nil slice")
			}
			ids := make([]string, len(got))
			for i, e := range got {
				ids[i] = e.ID
			}
			if !slices.Equal(ids, tt.want) && !(len(ids) == 0 && len(tt.want) == 0) {
				t.Errorf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestListSortsByStart(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestService(t)

	day := func(hour int) time.Time {
		return time.Date(2026, 8, 17, hour, 0, 0, 0, time.UTC)
	}
	// Written out of chronological order; List must still hand them back sorted.
	seedEntry(t, s, "c", day(15), day(16))
	seedEntry(t, s, "a", day(9), day(10))
	seedEntry(t, s, "b", day(12), day(13))

	got, err := s.List(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d entries, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Start.Before(got[i-1].Start) {
			t.Fatalf("entries out of order: %v before %v", got[i-1].Start, got[i].Start)
		}
	}
}

func TestNilPublisherIsNotAnError(t *testing.T) {
	ctx := context.Background()
	lg := &fakeLog{}
	s := NewService(lg, nil, core.FixedClock(testNow))

	start := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	view, err := s.Add(ctx, "acme", "note", nil, start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.Edit(ctx, view.ID, Patch{Note: ptr("edited")}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := s.Delete(ctx, view.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := len(lg.all()); got != 3 {
		t.Errorf("appended %d events, want 3", got)
	}
}

func TestLoadErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	lg := &fakeLog{loadErr: errors.New("db closed")}
	pub := &fakePub{}
	s := NewService(lg, pub, core.FixedClock(testNow))

	if _, err := s.Edit(ctx, "id", Patch{Note: ptr("x")}); err == nil {
		t.Error("Edit succeeded, want a load error")
	}
	if err := s.Delete(ctx, "id"); err == nil {
		t.Error("Delete succeeded, want a load error")
	}
	if _, err := s.List(ctx, time.Time{}, time.Time{}); err == nil {
		t.Error("List succeeded, want a load error")
	}
	if got := len(pub.snapshot()); got != 0 {
		t.Errorf("published %d times, want 0", got)
	}
}

// ptr returns a pointer to v, for building sparse patches inline.
func ptr[T any](v T) *T { return &v }
