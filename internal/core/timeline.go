package core

import (
	"fmt"
	"sort"
	"time"
)

// Timeline is the canonical fold of an event log: every entry, project and rate
// resolved to its state as of the last event replayed.
//
// It exists so that replay is written once. Three read models (day summary,
// range report, invoice) all need "what entries exist and how long are they",
// and three independent reconstructions of that from raw events would be three
// chances to disagree about the same log. Read models start from a Timeline and
// then project it their own way, which is where they are allowed to differ.
type Timeline struct {
	// Entries holds every surviving entry, sorted by Start then ID. The ID
	// tiebreak keeps the order total, so two entries starting in the same
	// clock tick do not shuffle between runs.
	Entries []EntryView

	// Running points into Entries at the open interval, or is nil.
	Running *EntryView

	// Projects is keyed by slug.
	Projects map[string]Project

	// Rates and Currencies are keyed by project slug, holding the most recent
	// RateSet. A project with no rate is absent from both.
	Rates      map[string]int64
	Currencies map[string]string

	// Seq is the position of the last event folded, so a caller can ask the
	// store for only what arrived since.
	Seq int64
}

// BuildTimeline replays events into a Timeline. Events must arrive in Seq
// order; the store guarantees that, and folding them out of order would apply
// an edit before the entry it edits exists.
//
// now closes any still-running interval for the purpose of Duration only — the
// entry stays Running and keeps a zero End, so a caller can tell "2h so far"
// from "2h, finished".
func BuildTimeline(events []Event, now time.Time) (Timeline, error) {
	tl := Timeline{
		Projects:   make(map[string]Project),
		Rates:      make(map[string]int64),
		Currencies: make(map[string]string),
	}

	// Entries are collected in a map because edits and deletes arrive by ID
	// out of positional order; ordering is imposed once at the end.
	entries := make(map[string]*EntryView)

	for _, e := range events {
		tl.Seq = e.Seq

		switch e.Kind {
		case KindProjectCreated:
			p, err := DecodePayload[ProjectCreated](e)
			if err != nil {
				return Timeline{}, err
			}
			tl.Projects[p.Slug] = Project{Slug: p.Slug, Name: p.Name}

		case KindProjectArchived:
			p, err := DecodePayload[ProjectArchived](e)
			if err != nil {
				return Timeline{}, err
			}
			// Archiving a slug the log never created would mean the write
			// model let an invalid command through; fold defensively rather
			// than resurrecting a phantom project with an empty name.
			if existing, ok := tl.Projects[p.Slug]; ok {
				existing.Archived = true
				tl.Projects[p.Slug] = existing
			}

		case KindTimerStarted:
			p, err := DecodePayload[TimerStarted](e)
			if err != nil {
				return Timeline{}, err
			}
			entries[p.EntryID] = &EntryView{
				ID:      p.EntryID,
				Project: p.Project,
				Note:    p.Note,
				Tags:    p.Tags,
				Start:   p.StartedAt,
				Running: true,
			}

		case KindTimerStopped:
			p, err := DecodePayload[TimerStopped](e)
			if err != nil {
				return Timeline{}, err
			}
			if ev, ok := entries[p.EntryID]; ok {
				ev.End = p.StoppedAt
				ev.Running = false
			}

		case KindEntryAdded:
			p, err := DecodePayload[EntryAdded](e)
			if err != nil {
				return Timeline{}, err
			}
			entries[p.EntryID] = &EntryView{
				ID:      p.EntryID,
				Project: p.Project,
				Note:    p.Note,
				Tags:    p.Tags,
				Start:   p.Start,
				End:     p.End,
			}

		case KindEntryEdited:
			p, err := DecodePayload[EntryEdited](e)
			if err != nil {
				return Timeline{}, err
			}
			ev, ok := entries[p.EntryID]
			if !ok {
				// Editing a deleted entry is a no-op, not an error: the
				// tombstone won and replay must stay total.
				continue
			}
			if p.Project != nil {
				ev.Project = *p.Project
			}
			if p.Note != nil {
				ev.Note = *p.Note
			}
			if p.Tags != nil {
				ev.Tags = *p.Tags
			}
			if p.Start != nil {
				ev.Start = *p.Start
			}
			if p.End != nil {
				ev.End = *p.End
				ev.Running = false
			}

		case KindEntryDeleted:
			p, err := DecodePayload[EntryDeleted](e)
			if err != nil {
				return Timeline{}, err
			}
			delete(entries, p.EntryID)

		case KindRateSet:
			p, err := DecodePayload[RateSet](e)
			if err != nil {
				return Timeline{}, err
			}
			tl.Rates[p.Project] = p.CentsPerHour
			tl.Currencies[p.Project] = p.Currency

		default:
			// An unknown Kind means the log was written by a newer binary.
			// Fail loudly: silently skipping would render a total that is
			// quietly wrong, which is worse than not rendering at all.
			return Timeline{}, fmt.Errorf("core: unknown event kind %q at seq %d: %w", e.Kind, e.Seq, ErrInvalid)
		}
	}

	tl.Entries = make([]EntryView, 0, len(entries))
	for _, ev := range entries {
		ev.Duration = entryDuration(*ev, now)
		tl.Entries = append(tl.Entries, *ev)
	}
	sort.Slice(tl.Entries, func(i, j int) bool {
		if tl.Entries[i].Start.Equal(tl.Entries[j].Start) {
			return tl.Entries[i].ID < tl.Entries[j].ID
		}
		return tl.Entries[i].Start.Before(tl.Entries[j].Start)
	})

	// Running is resolved after sorting so it points at the copy in Entries
	// rather than the scratch map's pointer.
	for i := range tl.Entries {
		if tl.Entries[i].Running {
			running := tl.Entries[i]
			tl.Running = &running
			break
		}
	}

	return tl, nil
}

// entryDuration measures an interval, treating a running entry as ending now.
// A negative span (a clock that jumped backwards, or an edit that moved Start
// past End) clamps to zero rather than subtracting from the day's total.
func entryDuration(ev EntryView, now time.Time) time.Duration {
	end := ev.End
	if ev.Running {
		end = now
	}
	d := end.Sub(ev.Start)
	if d < 0 {
		return 0
	}
	return d
}

// Bucketize turns a key→duration map into sorted Buckets with shares computed
// against total. Ordering is longest first, then by key, so a rendered table is
// stable across runs. Every grouped view in the system goes through here, which
// is why "by project" and "by tag" cannot drift apart.
func Bucketize(byKey map[string]time.Duration, total time.Duration) []Bucket {
	buckets := make([]Bucket, 0, len(byKey))
	for k, d := range byKey {
		var share float64
		if total > 0 {
			share = float64(d) / float64(total)
		}
		buckets = append(buckets, Bucket{Key: k, Duration: d, Share: share})
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].Duration == buckets[j].Duration {
			return buckets[i].Key < buckets[j].Key
		}
		return buckets[i].Duration > buckets[j].Duration
	})
	return buckets
}
