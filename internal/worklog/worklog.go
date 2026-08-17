// Package worklog is the write model for manually logged work entries: time
// entered after the fact, bypassing the timer.
//
// It is the single writer for entry.added, entry.edited and entry.deleted. It
// holds no state of its own — every command that needs to know what exists
// first replays the log through core.BuildTimeline, decides, and appends. That
// is what keeps this package a peer of internal/timer rather than a cache of it:
// two writers folding the same log cannot disagree about what an entry is.
//
// Two rules govern every command here.
//
// Persist, then publish. A notification is only ever sent after core.Log.Append
// has returned nil. Publishing first would let a subscriber re-read the log,
// miss the event that has not landed yet, and cache the absence; on an append
// error nothing is published at all.
//
// Patches are sparse. A nil field in a Patch means "leave alone" and is omitted
// from the recorded core.EntryEdited entirely, so that "clear the note"
// (a pointer to "") stays distinguishable from "do not touch the note" (nil)
// when the log is replayed years from now.
package worklog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// Patch is a sparse update: a nil field means "leave alone".
//
// Every field is a pointer precisely so that "set this to the zero value" and
// "do not touch this" are different values rather than the same empty string.
type Patch struct {
	Project *string
	Note    *string
	Tags    *[]string
	Start   *time.Time
	End     *time.Time
}

// isEmpty reports whether p sets no field at all.
func (p Patch) isEmpty() bool {
	return p.Project == nil && p.Note == nil && p.Tags == nil && p.Start == nil && p.End == nil
}

// Service is the single writer for manually-logged work entries.
type Service struct {
	log   core.Log
	pub   core.Publisher
	clock core.Clock
}

// NewService returns a Service backed by log, publishing to pub, timestamping
// with clock.
//
// pub may be nil: a Service with nobody listening is a valid Service, not an
// error, which is what lets a one-shot CLI command skip wiring a bus. A nil
// clock falls back to core.SystemClock so that a Service is never a latent
// panic.
func NewService(log core.Log, pub core.Publisher, clock core.Clock) *Service {
	if clock == nil {
		clock = core.SystemClock
	}
	return &Service{log: log, pub: pub, clock: clock}
}

// Add records a complete interval logged after the fact.
//
// project must be non-empty once trimmed and end must be strictly after start;
// both failures wrap core.ErrInvalid. Times are stored in UTC because the log is
// canonical storage and must not carry the machine's offset. Tags are
// normalised: trimmed, lowercased, de-duplicated and sorted, with an empty
// result stored as nil.
func (s *Service) Add(ctx context.Context, project, note string, tags []string, start, end time.Time) (core.EntryView, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return core.EntryView{}, fmt.Errorf("worklog: add: project is empty: %w", core.ErrInvalid)
	}
	if !end.After(start) {
		return core.EntryView{}, fmt.Errorf("worklog: add %q: end %s is not after start %s: %w",
			project, end.Format(time.RFC3339), start.Format(time.RFC3339), core.ErrInvalid)
	}
	start, end = start.UTC(), end.UTC()

	id, err := core.NewID()
	if err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: add %q: %w", project, err)
	}
	tags = normalizeTags(tags)

	ev, err := core.NewEvent(core.KindEntryAdded, core.EntryAggregate(id), s.clock(), core.EntryAdded{
		EntryID: id,
		Project: project,
		Note:    note,
		Tags:    tags,
		Start:   start,
		End:     end,
	})
	if err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: add %q: %w", project, err)
	}
	if err := s.log.Append(ctx, ev); err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: add %q: append: %w", project, err)
	}
	s.publish(id)

	// No fold is needed here: the event that was just written fully determines
	// the new entry, and a complete interval's duration is its own span.
	return core.EntryView{
		ID:       id,
		Project:  project,
		Note:     note,
		Tags:     tags,
		Start:    start,
		End:      end,
		Duration: end.Sub(start),
	}, nil
}

// Edit applies a sparse patch to an existing entry.
//
// An unknown or already-tombstoned entryID wraps core.ErrNotFound. The patch is
// validated against its result, not against itself: if applying it would leave
// End at or before Start the command wraps core.ErrInvalid and appends nothing.
// A running entry may be edited only by a patch that touches neither Start nor
// End — a running interval's bounds belong to the timer aggregate — otherwise
// the command wraps core.ErrConflict. A patch with no fields set is a no-op.
func (s *Service) Edit(ctx context.Context, entryID string, p Patch) (core.EntryView, error) {
	events, err := s.log.Load(ctx, 0)
	if err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: load: %w", entryID, err)
	}
	now := s.clock()
	tl, err := core.BuildTimeline(events, now)
	if err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: replay: %w", entryID, err)
	}
	current, ok := findEntry(tl.Entries, entryID)
	if !ok {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: %w", entryID, core.ErrNotFound)
	}

	// An empty patch appends nothing. An event that changes no field is noise in
	// an append-only log: every future replay would pay to decode it and then
	// apply nothing. Returning the entry as-is is the honest answer.
	if p.isEmpty() {
		return current, nil
	}

	// A running interval is owned by the timer aggregate — the stop event is
	// what closes it. Letting this writer move those bounds would give one
	// entry two writers and a race no ordering rule can settle.
	if current.Running && (p.Start != nil || p.End != nil) {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: entry is running: %w", entryID, core.ErrConflict)
	}

	// Only the fields the caller actually set are carried into the payload;
	// the rest stay nil so replay knows they were never touched.
	edit := core.EntryEdited{EntryID: entryID}
	start, end := current.Start, current.End
	if p.Project != nil {
		project := strings.TrimSpace(*p.Project)
		if project == "" {
			return core.EntryView{}, fmt.Errorf("worklog: edit %q: project is empty: %w", entryID, core.ErrInvalid)
		}
		edit.Project = &project
	}
	if p.Note != nil {
		note := *p.Note
		edit.Note = &note
	}
	if p.Tags != nil {
		tags := normalizeTags(*p.Tags)
		if tags == nil {
			// A set-but-empty Tags patch means "clear the tags", and that
			// intent has to survive JSON: a non-nil pointer to a nil slice
			// marshals as null, which decodes back to a nil pointer — i.e. it
			// would replay as "leave alone". An empty slice round-trips as [].
			tags = []string{}
		}
		edit.Tags = &tags
	}
	if p.Start != nil {
		start = p.Start.UTC()
		edit.Start = &start
	}
	if p.End != nil {
		end = p.End.UTC()
		edit.End = &end
	}

	// Validate the post-patch interval, not the patch: moving only Start can
	// invert an interval whose End the caller never mentioned. A running entry
	// is exempt because its End is legitimately zero until the timer stops it.
	if !current.Running && !end.After(start) {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: end %s is not after start %s: %w",
			entryID, end.Format(time.RFC3339), start.Format(time.RFC3339), core.ErrInvalid)
	}

	ev, err := core.NewEvent(core.KindEntryEdited, core.EntryAggregate(entryID), now, edit)
	if err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: %w", entryID, err)
	}
	if err := s.log.Append(ctx, ev); err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: append: %w", entryID, err)
	}
	s.publish(entryID)

	// Replay the log plus the event just written rather than patching the view
	// by hand, so the caller sees exactly what any reader will see. The copy
	// avoids appending into a slice the store may still own.
	replayed := make([]core.Event, 0, len(events)+1)
	replayed = append(replayed, events...)
	replayed = append(replayed, ev)
	tl, err = core.BuildTimeline(replayed, now)
	if err != nil {
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: replay: %w", entryID, err)
	}
	updated, ok := findEntry(tl.Entries, entryID)
	if !ok {
		// Unreachable: the same fold saw this entry one event ago.
		return core.EntryView{}, fmt.Errorf("worklog: edit %q: entry vanished from replay: %w", entryID, core.ErrNotFound)
	}
	return updated, nil
}

// Delete tombstones an entry.
//
// An unknown or already-tombstoned entryID wraps core.ErrNotFound. The original
// events stay in the log: deletion is an assertion that replay must honour, not
// an erasure.
func (s *Service) Delete(ctx context.Context, entryID string) error {
	events, err := s.log.Load(ctx, 0)
	if err != nil {
		return fmt.Errorf("worklog: delete %q: load: %w", entryID, err)
	}
	tl, err := core.BuildTimeline(events, s.clock())
	if err != nil {
		return fmt.Errorf("worklog: delete %q: replay: %w", entryID, err)
	}
	if _, ok := findEntry(tl.Entries, entryID); !ok {
		return fmt.Errorf("worklog: delete %q: %w", entryID, core.ErrNotFound)
	}

	ev, err := core.NewEvent(core.KindEntryDeleted, core.EntryAggregate(entryID), s.clock(), core.EntryDeleted{
		EntryID: entryID,
	})
	if err != nil {
		return fmt.Errorf("worklog: delete %q: %w", entryID, err)
	}
	if err := s.log.Append(ctx, ev); err != nil {
		return fmt.Errorf("worklog: delete %q: append: %w", entryID, err)
	}
	s.publish(entryID)
	return nil
}

// List returns entries starting within [from, to), sorted by start.
//
// The bounds are half-open so that adjacent ranges tile without double-counting
// an entry that starts exactly on the seam. A zero from means no lower bound and
// a zero to means no upper bound. The result is never nil, and a running entry
// is included when its start falls in range.
func (s *Service) List(ctx context.Context, from, to time.Time) ([]core.EntryView, error) {
	events, err := s.log.Load(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("worklog: list: load: %w", err)
	}
	tl, err := core.BuildTimeline(events, s.clock())
	if err != nil {
		return nil, fmt.Errorf("worklog: list: replay: %w", err)
	}

	// core.BuildTimeline already sorts by Start then ID; filtering in place
	// preserves that order, so no second sort is needed.
	out := make([]core.EntryView, 0, len(tl.Entries))
	for _, e := range tl.Entries {
		if !from.IsZero() && e.Start.Before(from) {
			continue
		}
		if !to.IsZero() && !e.Start.Before(to) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// publish announces that entryID changed. It is called only after a successful
// Append: a subscriber reacts by re-reading the log, so a notification sent
// before the event lands would teach it the change does not exist.
func (s *Service) publish(entryID string) {
	if s.pub == nil {
		return
	}
	s.pub.Publish(core.SubjectEntries, entryID)
}

// findEntry looks up an entry in a replayed timeline. A missing id means either
// "never existed" or "tombstoned"; both are core.ErrNotFound to a caller,
// because a deleted entry is not a thing you can edit.
func findEntry(entries []core.EntryView, id string) (core.EntryView, bool) {
	for _, e := range entries {
		if e.ID == id {
			return e, true
		}
	}
	return core.EntryView{}, false
}

// normalizeTags trims, lowercases, de-duplicates and sorts tags, returning nil
// when nothing survives.
//
// Normalising on the way in rather than on the way out means the log holds one
// spelling of each tag forever: read models group by exact string, so "Deep
// Work" and "deep work" landing as two buckets would be a reporting bug written
// permanently into storage.
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
	sort.Strings(out)
	return out
}
