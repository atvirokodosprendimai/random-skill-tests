// Package timer is the write model for tempo's running-timer aggregate.
//
// It is the single writer for that aggregate: every command that opens, closes
// or discards an interval goes through Service. That is what makes tempo's
// headline invariant — at most one timer running at a time — enforceable at
// all. A second writer would be a second chance to open a concurrent interval,
// and no amount of checking here would catch it.
//
// The package holds no state of its own. Current state is re-derived on every
// command by replaying the log through core.BuildTimeline, because the log is
// the only truth and a cached "a timer is running" flag is precisely the drift
// CQRS exists to avoid.
package timer

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// Service is the single writer for the running-timer aggregate.
//
// Every command follows the same three beats: replay the log to see current
// state, append one event, then publish. Serialising concurrent callers is the
// surrounding process's job — the check-then-append is not atomic, so two
// goroutines sharing a Service could both observe an idle timeline and both
// start. In tempo that is the CLI, which runs one command per invocation.
type Service struct {
	log   core.Log
	pub   core.Publisher
	clock core.Clock
}

// NewService returns a Service backed by log, publishing to pub, timestamping
// with clock.
//
// pub may be nil. A CLI run has no bus to fan out to, and that is an ordinary
// configuration rather than an error; publishing simply becomes a no-op.
func NewService(log core.Log, pub core.Publisher, clock core.Clock) *Service {
	return &Service{log: log, pub: pub, clock: clock}
}

// Start opens a new interval on project.
//
// It fails with core.ErrConflict if a timer is already running and appends
// nothing in that case: the at-most-one-running invariant is the reason this
// package exists, and a refused command must leave no trace in the log.
//
// Start deliberately does not check that project exists. Projects are a
// separate aggregate with their own writer, and validating across the two here
// would couple two write models — this one would start failing for reasons
// that live in another aggregate's history. Replay treats an entry's project
// as a free label; resolving it against known projects is the read side's job.
//
// note is free text and may be empty. tags are normalised (trimmed,
// lowercased, de-duplicated, sorted) so two spellings of the same set compare
// equal in every downstream read model.
func (s *Service) Start(ctx context.Context, project, note string, tags []string) (core.EntryView, error) {
	slug := strings.TrimSpace(project)
	if slug == "" {
		return core.EntryView{}, fmt.Errorf("timer: start %q: project is empty: %w", project, core.ErrInvalid)
	}

	// One clock reading per command: the same instant times the event, stamps
	// the payload and measures the timeline, so nothing can disagree by a tick.
	// UTC because the log is canonical storage, matching core.NewEvent.
	now := s.clock().UTC()

	running, err := s.runningAt(ctx, now)
	if err != nil {
		return core.EntryView{}, fmt.Errorf("timer: start %q: %w", slug, err)
	}
	if running != nil {
		return core.EntryView{}, fmt.Errorf("timer: start %q: %q is already running: %w", slug, running.Project, core.ErrConflict)
	}

	id, err := core.NewID()
	if err != nil {
		return core.EntryView{}, fmt.Errorf("timer: start %q: %w", slug, err)
	}
	normalised := normalizeTags(tags)

	ev, err := core.NewEvent(core.KindTimerStarted, core.EntryAggregate(id), now, core.TimerStarted{
		EntryID:   id,
		Project:   slug,
		Note:      note,
		Tags:      normalised,
		StartedAt: now,
	})
	if err != nil {
		return core.EntryView{}, fmt.Errorf("timer: start %q: %w", slug, err)
	}
	if err := s.log.Append(ctx, ev); err != nil {
		// Persist, then publish. Nothing landed, so nothing is announced —
		// publishing here would let a subscriber re-read and render a state
		// that no write produced.
		return core.EntryView{}, fmt.Errorf("timer: start %q: append: %w", slug, err)
	}
	s.publish(id)

	// Duration is zero by construction: the interval starts at now.
	return core.EntryView{
		ID:      id,
		Project: slug,
		Note:    note,
		Tags:    normalised,
		Start:   now,
		Running: true,
	}, nil
}

// Stop closes the running interval and returns it completed, with End set,
// Duration filled in and Running false.
//
// It fails with core.ErrNotFound if no timer is running.
func (s *Service) Stop(ctx context.Context) (core.EntryView, error) {
	now := s.clock().UTC()

	running, err := s.runningAt(ctx, now)
	if err != nil {
		return core.EntryView{}, fmt.Errorf("timer: stop: %w", err)
	}
	if running == nil {
		return core.EntryView{}, fmt.Errorf("timer: stop: no timer is running: %w", core.ErrNotFound)
	}

	ev, err := core.NewEvent(core.KindTimerStopped, core.EntryAggregate(running.ID), now, core.TimerStopped{
		EntryID:   running.ID,
		StoppedAt: now,
	})
	if err != nil {
		return core.EntryView{}, fmt.Errorf("timer: stop %q: %w", running.ID, err)
	}
	if err := s.log.Append(ctx, ev); err != nil {
		return core.EntryView{}, fmt.Errorf("timer: stop %q: append: %w", running.ID, err)
	}
	s.publish(running.ID)

	// The completed view is the running one closed at now. Duration needs no
	// recomputing: BuildTimeline already measured Start→now against this same
	// instant, clamp on a backwards clock included.
	done := *running
	done.End = now
	done.Running = false
	return done, nil
}

// Running reports the open interval, if any.
//
// The returned view's Duration is measured from its start to the Service
// clock's current time, so a caller can render "2h so far" without doing the
// arithmetic itself.
func (s *Service) Running(ctx context.Context) (core.EntryView, bool, error) {
	now := s.clock().UTC()

	running, err := s.runningAt(ctx, now)
	if err != nil {
		return core.EntryView{}, false, fmt.Errorf("timer: running: %w", err)
	}
	if running == nil {
		return core.EntryView{}, false, nil
	}
	return *running, true, nil
}

// Cancel discards the running interval without recording any time.
//
// It appends core.KindEntryDeleted rather than core.KindTimerStopped, because
// a cancelled timer must contribute zero time to every read model. Stopping it
// instead would leave a short but real entry that still shows up in the day's
// list and in an invoice; the tombstone removes it from replay entirely.
//
// It fails with core.ErrNotFound if no timer is running.
func (s *Service) Cancel(ctx context.Context) error {
	now := s.clock().UTC()

	running, err := s.runningAt(ctx, now)
	if err != nil {
		return fmt.Errorf("timer: cancel: %w", err)
	}
	if running == nil {
		return fmt.Errorf("timer: cancel: no timer is running: %w", core.ErrNotFound)
	}

	ev, err := core.NewEvent(core.KindEntryDeleted, core.EntryAggregate(running.ID), now, core.EntryDeleted{
		EntryID: running.ID,
	})
	if err != nil {
		return fmt.Errorf("timer: cancel %q: %w", running.ID, err)
	}
	if err := s.log.Append(ctx, ev); err != nil {
		return fmt.Errorf("timer: cancel %q: append: %w", running.ID, err)
	}
	s.publish(running.ID)
	return nil
}

// runningAt replays the whole log and returns the open interval as of now, or
// nil when the timer is idle.
//
// Every command re-derives this instead of caching it: another process may
// have appended since the last call, and this package refuses to be the one
// that decides state from memory. core.BuildTimeline owns the fold, so the
// write model and the read models can never disagree about what is running.
func (s *Service) runningAt(ctx context.Context, now time.Time) (*core.EntryView, error) {
	events, err := s.log.Load(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("load log: %w", err)
	}
	tl, err := core.BuildTimeline(events, now)
	if err != nil {
		return nil, fmt.Errorf("replay log: %w", err)
	}
	return tl.Running, nil
}

// publish announces a landed write on core.SubjectTimer.
//
// It is only ever reached after a successful Append — the persist-then-publish
// ordering is the single most important rule in this codebase. The payload is
// the entry id, never rendered state, so a subscriber always re-reads the log
// and a superseded notification is safe to drop.
func (s *Service) publish(entryID string) {
	if s.pub == nil {
		return
	}
	s.pub.Publish(core.SubjectTimer, entryID)
}

// normalizeTags trims, lowercases, drops empties, de-duplicates and sorts tags.
//
// Normalising on the way in rather than on the way out means "Go", " go " and
// "GO" are one tag in every read model without any of them re-deriving the
// rule — grouping by tag can then compare strings and be right. An empty
// result is nil rather than an empty slice, so a tagless entry round-trips
// through the payload's omitempty encoding unchanged.
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return out
}
