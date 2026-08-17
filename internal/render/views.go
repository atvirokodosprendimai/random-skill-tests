package render

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// Fixed column widths and the gap between columns. These hold values of a known
// shape, so giving them slack would only steal it from the one column that can
// use it — the note.
const (
	gapWidth  = 2  // breathing room between every pair of columns
	markWidth = 2  // "▶ " on a running entry, blank otherwise
	spanWidth = 11 // "09:00–12:30", and "14:05–now" padded to match
	dateWidth = 6  // "02 Jan"
	dayWidth  = 10 // "Mon 02 Jan"
	durWidth  = 7  // "25h 00m"; a four-digit hour count overruns by design
	pctWidth  = 4  // "100%"
	moneyCol  = 12 // "-€1,234,567.89" overruns; realistic invoice figures do not
	minCol    = 6  // the narrowest a flexible text column may be squeezed
	maxNameW  = 18 // a name column wider than this is wasted on project slugs
)

// Date layouts. Kept together so the five renderers cannot drift apart.
const (
	layoutDayTitle = "Mon 02 Jan 2006"
	layoutRange    = "02 Jan 2006"
	layoutDay      = "Mon 02 Jan"
	layoutDate     = "02 Jan"
	layoutClock    = "15:04"
)

// Day writes a single day's view.
func (r *Renderer) Day(v core.DayView) error {
	e := &errWriter{w: r.w}
	r.title(e, v.Date.Local().Format(layoutDayTitle), Duration(v.Total))
	r.rule(e)

	if len(v.Entries) == 0 {
		r.empty(e, "no entries for this day — start one with 'tempo start <project>'")
		return e.err
	}

	cols := entryLayout(r.width, v.Entries, false)
	for _, en := range v.Entries {
		r.entryRow(e, cols, en)
	}
	r.buckets(e, "By project", v.ByProject)
	r.buckets(e, "By tag", v.ByTag)

	// Running is redundant with the ▶ row above — by design, so a renderer that
	// ignores it still shows the right total. It earns its place here as the
	// reminder that the day is not finished yet.
	if v.Running != nil {
		e.line("")
		e.line("  " + r.accent("▶") + r.dim(" timer still running — stop it with 'tempo stop'"))
	}
	return e.err
}

// Report writes a range rollup.
func (r *Renderer) Report(v core.RangeReport) error {
	e := &errWriter{w: r.w}
	r.title(e, dateRange(v.From, v.To), Duration(v.Total))
	r.rule(e)

	if len(v.Days) == 0 && len(v.ByProject) == 0 && len(v.ByTag) == 0 {
		r.empty(e, "no time tracked in this range — track some with 'tempo start <project>'")
		return e.err
	}

	r.daily(e, v.Days)
	r.buckets(e, "By project", v.ByProject)
	r.buckets(e, "By tag", v.ByTag)
	return e.err
}

// Invoice writes a billable invoice.
func (r *Renderer) Invoice(v core.Invoice) error {
	e := &errWriter{w: r.w}

	// An invoice with no project is an empty-state placeholder, not a document;
	// naming the hole keeps the remedy line below grammatical.
	project := v.Project
	if project == "" {
		project = "<project>"
	}

	r.title(e, "Invoice · "+project, dateRange(v.From, v.To))
	r.rule(e)

	if v.CentsPerHour == 0 {
		e.line("  " + r.dim("no rate set — 'tempo rate "+project+" <amount>'"))
	} else {
		e.line("  " + r.dim(Money(v.CentsPerHour, v.Currency)+" per hour"))
	}

	if len(v.Lines) == 0 {
		e.line("")
		r.empty(e, "nothing billable in this range — log time with 'tempo start "+project+"'")
		return e.err
	}

	// notes is the only flexible column; everything else is fixed above.
	notes := r.width - (gapWidth + dateWidth + gapWidth + gapWidth + durWidth + gapWidth + moneyCol)
	if notes < minCol {
		notes = minCol
	}

	e.line("")
	e.line(r.dim("  " + padRight("Date", dateWidth) + "  " + padRight("Notes", notes) +
		"  " + padLeft("Hours", durWidth) + "  " + padLeft("Amount", moneyCol)))
	for _, ln := range v.Lines {
		e.line("  " + r.dim(padRight(ln.Date.Local().Format(layoutDate), dateWidth)) +
			"  " + padRight(joinNotes(ln.Notes), notes) +
			"  " + padLeft(Duration(ln.Duration), durWidth) +
			"  " + padLeft(Money(ln.AmountCents, v.Currency), moneyCol))
	}

	r.rule(e)
	e.line("  " + padRight("Total", dateWidth+gapWidth+notes) +
		"  " + padLeft(Duration(v.Total), durWidth) +
		"  " + r.accent(padLeft(Money(v.TotalCents, v.Currency), moneyCol)))
	return e.err
}

// Projects writes a project list.
func (r *Renderer) Projects(ps []core.Project) error {
	e := &errWriter{w: r.w}

	var active, archived int
	for _, p := range ps {
		if p.Archived {
			archived++
		} else {
			active++
		}
	}
	headline := ""
	if len(ps) > 0 {
		headline = plural(active, "active", "active")
		if archived > 0 {
			headline += " · " + plural(archived, "archived", "archived")
		}
	}

	r.title(e, "Projects", headline)
	r.rule(e)

	if len(ps) == 0 {
		r.empty(e, "no projects yet — create one with 'tempo project add <slug>'")
		return e.err
	}

	slugCol := minCol
	for _, p := range ps {
		if n := utf8.RuneCountInString(p.Slug); n > slugCol {
			slugCol = n
		}
	}
	slugCol = min(slugCol, maxNameW)
	nameCol := r.width - (gapWidth + slugCol + gapWidth + gapWidth + len("archived"))
	if nameCol < minCol {
		nameCol = minCol
	}

	for _, p := range ps {
		row := "  " + padRight(p.Slug, slugCol) + "  "
		// Pad the name only when a status follows it: an archived-free list
		// should not trail whitespace into a pipe or a golden file.
		if p.Archived {
			row += r.dim(padRight(p.Name, nameCol)) + "  " + r.dim("archived")
		} else {
			row += r.dim(truncate(p.Name, nameCol))
		}
		e.line(row)
	}
	return e.err
}

// Entries writes a flat entry list.
func (r *Renderer) Entries(es []core.EntryView) error {
	e := &errWriter{w: r.w}

	headline := ""
	if len(es) > 0 {
		headline = plural(len(es), "entry", "entries")
	}
	r.title(e, "Entries", headline)
	r.rule(e)

	if len(es) == 0 {
		r.empty(e, "no entries yet — start one with 'tempo start <project>'")
		return e.err
	}

	// Unlike Day, this list can span days, so it carries a date column.
	cols := entryLayout(r.width, es, true)
	for _, en := range es {
		r.entryRow(e, cols, en)
	}
	return e.err
}

// entryCols is the resolved width of every flexible column in an entry table.
type entryCols struct {
	date    int // 0 when the list is already scoped to a single day
	project int
	note    int
}

// entryLayout splits the width budget across an entry table's columns.
//
// The fixed columns are subtracted first; then the project column is sized to
// the longest name actually present and every remaining cell goes to the note.
// Sizing to content rather than to a fixed share matters here because slugs are
// short and notes are not: a project column padded to a speculative width buys
// nothing and costs the note the room it needed. Both columns keep a floor, so
// an absurdly narrow budget makes the table overrun rather than collapse into a
// column of ellipses — an overrun is obvious and fixable, a shredded table is
// merely unreadable.
func entryLayout(width int, es []core.EntryView, withDate bool) entryCols {
	fixed := markWidth + spanWidth + 3*gapWidth + durWidth
	cols := entryCols{}
	if withDate {
		cols.date = dateWidth
		fixed += dateWidth + gapWidth
	}
	flex := width - fixed

	cols.project = minCol
	for _, en := range es {
		if n := utf8.RuneCountInString(en.Project); n > cols.project {
			cols.project = n
		}
	}
	cols.project = min(cols.project, maxNameW)
	cols.project = min(cols.project, max(flex-minCol, minCol))
	cols.note = max(flex-cols.project, minCol)
	return cols
}

// entryRow writes one entry line. A running entry is marked with ▶ and accented
// at both ends of the row — marker and elapsed time — so the eye lands on it
// before it starts reading; the "–now" in its span carries the same fact in
// text, for readers who have no colour.
func (r *Renderer) entryRow(e *errWriter, c entryCols, en core.EntryView) {
	mark := padRight("", markWidth)
	if en.Running {
		mark = padRight("▶", markWidth)
	}

	var b strings.Builder
	b.WriteString(r.accentIf(en.Running, mark))
	if c.date > 0 {
		b.WriteString(r.dim(padRight(en.Start.Local().Format(layoutDate), c.date)))
		b.WriteString("  ")
	}
	b.WriteString(r.dim(padRight(span(en), spanWidth)))
	b.WriteString("  ")
	b.WriteString(padRight(en.Project, c.project))
	b.WriteString("  ")
	b.WriteString(r.dim(padRight(describe(en), c.note)))
	b.WriteString("  ")
	b.WriteString(r.accentIf(en.Running, padLeft(Duration(en.Duration), durWidth)))
	e.line(b.String())
}

// buckets draws a grouped breakdown as proportional bars. An absent grouping is
// silently skipped: a "By tag" caption over nothing is noise, and the empty
// state that matters was already handled by the caller.
func (r *Renderer) buckets(e *errWriter, name string, bs []core.Bucket) {
	if len(bs) == 0 {
		return
	}
	r.section(e, name)

	keyCol := minCol
	for _, b := range bs {
		if n := utf8.RuneCountInString(b.Key); n > keyCol {
			keyCol = n
		}
	}
	// Bound the label column so one pathological project name cannot squeeze
	// the bars — the point of the section — down to nothing.
	keyCol = min(keyCol, max(r.width/4, minCol))

	for _, b := range bs {
		r.meterRow(e, keyCol, b.Key, b.Share, percent(b.Share), b.Duration)
	}
}

// daily draws the per-day strip of a range report.
//
// Its bars are scaled to the busiest day rather than to the range total, because
// the question a daily strip answers is "which days were heavy" — against the
// total, a twenty-day report is twenty bars all under 10% and indistinguishable.
// The percentage column is left blank for exactly that reason: a percentage of
// the peak is not a number anyone wants, and printing one against a bar drawn
// on a different scale would be a lie.
func (r *Renderer) daily(e *errWriter, ds []core.DayTotal) {
	if len(ds) == 0 {
		return
	}
	r.section(e, "Daily")

	var peak time.Duration
	for _, d := range ds {
		if d.Duration > peak {
			peak = d.Duration
		}
	}
	for _, d := range ds {
		var share float64
		if peak > 0 {
			share = float64(d.Duration) / float64(peak)
		}
		r.meterRow(e, dayWidth, d.Date.Local().Format(layoutDay), share, padLeft("", pctWidth), d.Duration)
	}
}

// meterRow writes one label + bar + figure line. Every bar section shares it so
// that the bars in "Daily", "By project" and "By tag" line up on the same left
// and right edges even though their labels differ in width.
func (r *Renderer) meterRow(e *errWriter, keyCol int, key string, share float64, pct string, d time.Duration) {
	fixed := gapWidth + keyCol + gapWidth + gapWidth + pctWidth + gapWidth + durWidth
	barCol := max(r.width-fixed, minCol+2)

	e.line("  " + padRight(key, keyCol) +
		"  " + r.accent(bar(share, barCol)) +
		"  " + r.dim(pct) +
		"  " + padLeft(Duration(d), durWidth))
}

// span renders an entry's clock range, e.g. "09:00–12:30".
//
// A running entry has no end, so it reads "14:05–now". The renderer must not
// substitute the current time: it has no clock by design, and the read model has
// already computed the elapsed Duration that the row's last column shows.
func span(en core.EntryView) string {
	start := en.Start.Local().Format(layoutClock)
	if en.Running || en.End.IsZero() {
		return start + "–now"
	}
	return start + "–" + en.End.Local().Format(layoutClock)
}

// describe joins an entry's note and tags into the one free-text column the
// table has room for. Tags follow the note because the note is what a reader
// scans for; when there is no note the tags stand in, so a labelled entry never
// shows a blank cell. An entry with neither gets an em dash rather than a hole.
func describe(en core.EntryView) string {
	parts := make([]string, 0, len(en.Tags)+1)
	if en.Note != "" {
		parts = append(parts, en.Note)
	}
	for _, t := range en.Tags {
		parts = append(parts, "#"+t)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}

// joinNotes collapses an invoice line's notes into one cell.
func joinNotes(notes []string) string {
	if len(notes) == 0 {
		return "—"
	}
	return strings.Join(notes, ", ")
}

// dateRange renders an inclusive span. Both ends carry their year: a client
// reading an invoice six months later should not have to infer one.
func dateRange(from, to time.Time) string {
	return from.Local().Format(layoutRange) + " – " + to.Local().Format(layoutRange)
}
