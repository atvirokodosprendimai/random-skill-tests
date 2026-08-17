package core

import "time"

// EntryView is one work interval resolved for display: every EntryEdited patch
// already applied, every tombstoned entry already gone. Read models consume
// these instead of raw events so that none of them has to know how an entry got
// to be the way it is.
type EntryView struct {
	ID       string
	Project  string
	Note     string
	Tags     []string
	Start    time.Time
	End      time.Time // zero while Running
	Duration time.Duration
	Running  bool
}

// Bucket is one slice of a total, used for every grouped breakdown (by project,
// by tag). Share is precomputed because the renderer draws bars from it and
// should not be doing arithmetic.
type Bucket struct {
	Key      string
	Duration time.Duration
	Share    float64 // 0..1 of the parent total
}

// DayTotal is one point on a daily sparkline.
type DayTotal struct {
	Date     time.Time
	Duration time.Duration
}

// DayView is the read model for a single day: the answer to "what did I do
// today". Running is nil unless a timer is open, and when set it also appears
// in Entries, so a renderer that ignores Running still shows the right total.
type DayView struct {
	Date      time.Time
	Total     time.Duration
	Running   *EntryView
	Entries   []EntryView
	ByProject []Bucket
	ByTag     []Bucket
}

// RangeReport is the read model for an arbitrary span. It is not a list of
// DayViews: rolling up is a different question than detailing, so it gets its
// own shape rather than making the renderer aggregate.
type RangeReport struct {
	From      time.Time
	To        time.Time
	Total     time.Duration
	Days      []DayTotal
	ByProject []Bucket
	ByTag     []Bucket
}

// InvoiceLine is one billable day of work on a project. Lines collapse a day's
// entries because that is the granularity a client is willing to read.
type InvoiceLine struct {
	Date        time.Time
	Notes       []string
	Duration    time.Duration
	AmountCents int64
}

// Invoice is the read model for billable work: the same log, enriched with the
// rate that applied, projected into money. Enrichment belongs here on the read
// side and never in the write path.
type Invoice struct {
	Project      string
	From         time.Time
	To           time.Time
	Currency     string
	CentsPerHour int64
	Total        time.Duration
	Lines        []InvoiceLine
	TotalCents   int64
}

// Project is a project as of the end of the log.
type Project struct {
	Slug     string
	Name     string
	Archived bool
}
