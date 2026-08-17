package render

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// The fixtures below build their times in time.Local so that the goldens are
// stable in any zone. The renderer localises through Time.Local — internal/core
// stores the log in UTC and declares that turning it back into wall-clock time
// is render's job — and Local() on a time already in time.Local is the identity,
// so a golden written here does not move when TZ does.
func at(hh, mm int) time.Time { return time.Date(2026, 8, 17, hh, mm, 0, 0, time.Local) }
func on(day int) time.Time    { return time.Date(2026, 8, day, 0, 0, 0, 0, time.Local) }

func mins(m int) time.Duration { return time.Duration(m) * time.Minute }

func sampleDay() core.DayView {
	running := core.EntryView{
		ID: "c3", Project: "tempo", Note: "writing the renderer",
		Tags: []string{"deep"}, Start: at(14, 5), Duration: mins(72), Running: true,
	}
	return core.DayView{
		Date:    on(17),
		Total:   mins(342),
		Running: &running,
		Entries: []core.EntryView{
			{ID: "a1", Project: "tempo", Note: "event store replay", Tags: []string{"deep", "oss"},
				Start: at(9, 0), End: at(12, 30), Duration: mins(210)},
			{ID: "b2", Project: "admin", Tags: []string{"ops"},
				Start: at(13, 0), End: at(14, 0), Duration: mins(60)},
			running,
		},
		ByProject: []core.Bucket{
			{Key: "tempo", Duration: mins(282), Share: 282.0 / 342.0},
			{Key: "admin", Duration: mins(60), Share: 60.0 / 342.0},
		},
		ByTag: []core.Bucket{
			{Key: "deep", Duration: mins(282), Share: 282.0 / 342.0},
			{Key: "oss", Duration: mins(210), Share: 210.0 / 342.0},
			{Key: "ops", Duration: mins(60), Share: 60.0 / 342.0},
		},
	}
}

func sampleReport() core.RangeReport {
	return core.RangeReport{
		From:  on(10),
		To:    on(14),
		Total: mins(1275),
		Days: []core.DayTotal{
			{Date: on(10), Duration: mins(390)},
			{Date: on(11), Duration: mins(285)},
			{Date: on(12), Duration: 0},
			{Date: on(13), Duration: mins(300)},
			{Date: on(14), Duration: mins(300)},
		},
		ByProject: []core.Bucket{
			{Key: "tempo", Duration: mins(855), Share: 855.0 / 1275.0},
			{Key: "café", Duration: mins(300), Share: 300.0 / 1275.0},
			{Key: "admin", Duration: mins(120), Share: 120.0 / 1275.0},
		},
		ByTag: []core.Bucket{
			{Key: "deep", Duration: mins(720), Share: 720.0 / 1275.0},
			{Key: "ops", Duration: mins(240), Share: 240.0 / 1275.0},
		},
	}
}

func sampleInvoice() core.Invoice {
	return core.Invoice{
		Project: "tempo", From: on(1), To: on(31),
		Currency: "EUR", CentsPerHour: 8500,
		Total: mins(705), TotalCents: 99875,
		Lines: []core.InvoiceLine{
			{Date: on(3), Notes: []string{"event store replay", "projections"}, Duration: mins(390), AmountCents: 55250},
			{Date: on(5), Notes: []string{"renderer"}, Duration: mins(195), AmountCents: 27625},
			{Date: on(12), Duration: mins(120), AmountCents: 17000},
		},
	}
}

func sampleProjects() []core.Project {
	return []core.Project{
		{Slug: "tempo", Name: "Tempo CLI"},
		{Slug: "café", Name: "Café rewrite"},
		{Slug: "admin", Name: "Admin & invoicing"},
		{Slug: "legacy", Name: "Legacy importer", Archived: true},
	}
}

func sampleEntries() []core.EntryView {
	return []core.EntryView{
		{ID: "a1", Project: "tempo", Note: "event store replay", Tags: []string{"deep"},
			Start: at(9, 0), End: at(12, 30), Duration: mins(210)},
		{ID: "b2", Project: "café", Note: "espresso machine telemetry ingest pipeline",
			Start: at(13, 0), End: at(14, 0), Duration: mins(60)},
		{ID: "c3", Project: "tempo", Note: "writing the renderer", Tags: []string{"deep"},
			Start: at(14, 5), Duration: mins(72), Running: true},
	}
}

// unicodeReport exercises rune-counted alignment: "café" and "café au lait" are
// longer in bytes than in runes, so a byte-padded column would come up short.
func unicodeReport() core.RangeReport {
	return core.RangeReport{
		From: on(10), To: on(14), Total: mins(600),
		ByProject: []core.Bucket{
			{Key: "café", Duration: mins(300), Share: 0.5},
			{Key: "café au lait", Duration: mins(180), Share: 0.3},
			{Key: "admin", Duration: mins(120), Share: 0.2},
		},
	}
}

// draw runs one renderer against a fresh buffer and returns everything written.
func draw(t *testing.T, opt Options, fn func(*Renderer) error) string {
	t.Helper()
	var buf bytes.Buffer
	if err := fn(New(&buf, opt)); err != nil {
		t.Fatalf("render returned %v", err)
	}
	return buf.String()
}

// want trims the leading newline that keeps a golden literal readable in source.
func want(golden string) string { return strings.TrimPrefix(golden, "\n") }

// views enumerates every golden case once, so the exact-output test, the
// no-escape test and the colour-transparency test all run over the same set.
var views = []struct {
	name string
	opt  Options
	run  func(*Renderer) error
	want string
}{
	{"day", Options{}, func(r *Renderer) error { return r.Day(sampleDay()) }, goldenDay},
	{"report", Options{}, func(r *Renderer) error { return r.Report(sampleReport()) }, goldenReport},
	{"invoice", Options{}, func(r *Renderer) error { return r.Invoice(sampleInvoice()) }, goldenInvoice},
	{"projects", Options{}, func(r *Renderer) error { return r.Projects(sampleProjects()) }, goldenProjects},
	{"entries", Options{}, func(r *Renderer) error { return r.Entries(sampleEntries()) }, goldenEntries},

	{"day/empty", Options{}, func(r *Renderer) error { return r.Day(core.DayView{Date: on(17)}) }, goldenDayEmpty},
	{"report/empty", Options{}, func(r *Renderer) error {
		return r.Report(core.RangeReport{From: on(10), To: on(14)})
	}, goldenReportEmpty},
	{"invoice/empty", Options{}, func(r *Renderer) error {
		return r.Invoice(core.Invoice{From: on(1), To: on(31)})
	}, goldenInvoiceEmpty},
	{"projects/empty", Options{}, func(r *Renderer) error { return r.Projects(nil) }, goldenProjectsEmpty},
	{"entries/empty", Options{}, func(r *Renderer) error { return r.Entries(nil) }, goldenEntriesEmpty},

	{"entries/narrow", Options{Width: 48}, func(r *Renderer) error { return r.Entries(sampleEntries()) }, goldenEntriesNarrow},
	{"report/unicode", Options{}, func(r *Renderer) error { return r.Report(unicodeReport()) }, goldenReportUnicode},
}

func TestViewsMatchGolden(t *testing.T) {
	for _, tc := range views {
		t.Run(tc.name, func(t *testing.T) {
			got := draw(t, tc.opt, tc.run)
			if got != want(tc.want) {
				t.Errorf("output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want(tc.want))
			}
		})
	}
}

// TestNoEscapesWithColorDisabled is the invariant every golden above depends on,
// and the one that makes `tempo day > log.txt` produce a readable file.
func TestNoEscapesWithColorDisabled(t *testing.T) {
	for _, tc := range views {
		t.Run(tc.name, func(t *testing.T) {
			if got := draw(t, tc.opt, tc.run); strings.ContainsRune(got, 0x1b) {
				t.Errorf("Color:false output contains an escape byte:\n%q", got)
			}
		})
	}
}

// TestColorIsPurelyAdditive proves colour carries no layout: stripping the
// escapes from a coloured render must give back the uncoloured render byte for
// byte. It fails the moment a cell is painted before it is padded, which is the
// one way ANSI can silently shear a column.
func TestColorIsPurelyAdditive(t *testing.T) {
	for _, tc := range views {
		t.Run(tc.name, func(t *testing.T) {
			plain := draw(t, tc.opt, tc.run)

			colorOpt := tc.opt
			colorOpt.Color = true
			colored := draw(t, colorOpt, tc.run)
			if !strings.ContainsRune(colored, 0x1b) {
				t.Fatal("Color:true emitted no escapes at all")
			}
			if got := stripANSI(colored); got != plain {
				t.Errorf("colour changed the layout\n--- stripped ---\n%s\n--- plain ---\n%s", got, plain)
			}
		})
	}
}

// TestEmptyStatesNameTheirCommand guards the rule that no empty state is allowed
// to be a dead end.
func TestEmptyStatesNameTheirCommand(t *testing.T) {
	empties := map[string]func(*Renderer) error{
		"day":      func(r *Renderer) error { return r.Day(core.DayView{Date: on(17)}) },
		"report":   func(r *Renderer) error { return r.Report(core.RangeReport{From: on(10), To: on(14)}) },
		"invoice":  func(r *Renderer) error { return r.Invoice(core.Invoice{From: on(1), To: on(31)}) },
		"projects": func(r *Renderer) error { return r.Projects(nil) },
		"entries":  func(r *Renderer) error { return r.Entries(nil) },
	}
	for name, fn := range empties {
		t.Run(name, func(t *testing.T) {
			got := draw(t, Options{}, fn)
			if !strings.Contains(got, "'tempo ") {
				t.Errorf("empty state does not name a command to run:\n%s", got)
			}
		})
	}
}

// TestAlignmentCountsRunesNotBytes is the discriminating test for the width
// algorithm. Every data row of a bar table is built from fixed-width cells, so
// all of them must be the same number of *runes* long. Padding by byte length
// would leave the "café" rows one column short and this would fail.
func TestAlignmentCountsRunesNotBytes(t *testing.T) {
	out := draw(t, Options{}, func(r *Renderer) error { return r.Report(unicodeReport()) })
	rows := strings.Split(strings.TrimSuffix(out, "\n"), "\n")[3:] // skip title, rule, caption

	if len(rows) != 3 {
		t.Fatalf("expected 3 bucket rows, got %d:\n%s", len(rows), out)
	}
	first := utf8.RuneCountInString(rows[0])
	for i, row := range rows {
		n := utf8.RuneCountInString(row)
		if n != first {
			t.Errorf("row %d is %d runes, row 0 is %d — columns are sheared:\n%s", i, n, first, out)
		}
		// Bars and shade blocks are multibyte on every row, so byte length must
		// exceed rune length; if it did not, this test would prove nothing.
		if len(row) == n {
			t.Errorf("row %d has no multibyte content, so it proves nothing: %q", i, row)
		}
	}
}

// TestNarrowWidthTruncatesInsteadOfWrapping checks the whole point of the width
// budget: at 48 columns nothing may spill onto a second line, and the long note
// must end in an ellipsis rather than run past the edge.
func TestNarrowWidthTruncatesInsteadOfWrapping(t *testing.T) {
	const width = 48
	out := draw(t, Options{Width: width}, func(r *Renderer) error { return r.Entries(sampleEntries()) })

	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > width {
			t.Errorf("line is %d columns, budget is %d: %q", n, width, line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected a truncation ellipsis at width %d:\n%s", width, out)
	}
	if strings.Contains(out, "espresso machine telemetry ingest pipeline") {
		t.Errorf("long note was not truncated:\n%s", out)
	}
}

// failWriter fails every write after the nth.
type failWriter struct {
	ok  int
	err error
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.ok == 0 {
		return 0, f.err
	}
	f.ok--
	return len(p), nil
}

// TestWriteErrorIsReturned checks that a broken pipe surfaces as an error rather
// than a panic or a silent truncation, from the first line and from mid-table.
func TestWriteErrorIsReturned(t *testing.T) {
	boom := errors.New("broken pipe")
	for _, ok := range []int{0, 1, 5} {
		w := &failWriter{ok: ok, err: boom}
		if err := New(w, Options{}).Day(sampleDay()); !errors.Is(err, boom) {
			t.Errorf("after %d good writes: got %v, want %v", ok, err, boom)
		}
	}
}

// stripANSI removes CSI sequences so a coloured render can be compared against
// an uncoloured one. It only has to understand the escapes this package emits.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			i++
			continue
		}
		for i < len(s) && s[i] != 'm' {
			i++
		}
		i++ // consume the 'm'
	}
	return b.String()
}

const goldenDay = `
Mon 17 Aug 2026                                                           5h 42m
────────────────────────────────────────────────────────────────────────────────
  09:00–12:30  tempo   event store replay #deep #oss                      3h 30m
  13:00–14:00  admin   #ops                                               1h 00m
▶ 14:05–now    tempo   writing the renderer #deep                         1h 12m

By project
  tempo   █████████████████████████████████████████████▍░░░░░░░░░   82%   4h 42m
  admin   █████████▋░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   18%   1h 00m

By tag
  deep    █████████████████████████████████████████████▍░░░░░░░░░   82%   4h 42m
  oss     █████████████████████████████████▊░░░░░░░░░░░░░░░░░░░░░   61%   3h 30m
  ops     █████████▋░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   18%   1h 00m

  ▶ timer still running — stop it with 'tempo stop'
`

const goldenReport = `
10 Aug 2026 – 14 Aug 2026                                                21h 15m
────────────────────────────────────────────────────────────────────────────────
Daily
  Mon 10 Aug  ███████████████████████████████████████████████████         6h 30m
  Tue 11 Aug  █████████████████████████████████████▎░░░░░░░░░░░░░         4h 45m
  Wed 12 Aug  ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░             0m
  Thu 13 Aug  ███████████████████████████████████████▎░░░░░░░░░░░         5h 00m
  Fri 14 Aug  ███████████████████████████████████████▎░░░░░░░░░░░         5h 00m

By project
  tempo   ████████████████████████████████████▉░░░░░░░░░░░░░░░░░░   67%  14h 15m
  café    █████████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   24%   5h 00m
  admin   █████▏░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░    9%   2h 00m

By tag
  deep    ███████████████████████████████░░░░░░░░░░░░░░░░░░░░░░░░   56%  12h 00m
  ops     ██████████▍░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   19%   4h 00m
`

const goldenInvoice = `
Invoice · tempo                                        01 Aug 2026 – 31 Aug 2026
────────────────────────────────────────────────────────────────────────────────
  €85.00 per hour

  Date    Notes                                              Hours        Amount
  03 Aug  event store replay, projections                   6h 30m       €552.50
  05 Aug  renderer                                          3h 15m       €276.25
  12 Aug  —                                                 2h 00m       €170.00
────────────────────────────────────────────────────────────────────────────────
  Total                                                    11h 45m       €998.75
`

const goldenProjects = `
Projects                                                   3 active · 1 archived
────────────────────────────────────────────────────────────────────────────────
  tempo   Tempo CLI
  café    Café rewrite
  admin   Admin & invoicing
  legacy  Legacy importer                                               archived
`

const goldenEntries = `
Entries                                                                3 entries
────────────────────────────────────────────────────────────────────────────────
  17 Aug  09:00–12:30  tempo   event store replay #deep                   3h 30m
  17 Aug  13:00–14:00  café    espresso machine telemetry ingest pipel…   1h 00m
▶ 17 Aug  14:05–now    tempo   writing the renderer #deep                 1h 12m
`

const goldenDayEmpty = `
Mon 17 Aug 2026                                                               0m
────────────────────────────────────────────────────────────────────────────────
  no entries for this day — start one with 'tempo start <project>'
`

const goldenReportEmpty = `
10 Aug 2026 – 14 Aug 2026                                                     0m
────────────────────────────────────────────────────────────────────────────────
  no time tracked in this range — track some with 'tempo start <project>'
`

const goldenInvoiceEmpty = `
Invoice · <project>                                    01 Aug 2026 – 31 Aug 2026
────────────────────────────────────────────────────────────────────────────────
  no rate set — 'tempo rate set <project> <amount>'

  nothing billable in this range — log time with 'tempo start <project>'
`

const goldenProjectsEmpty = `
Projects
────────────────────────────────────────────────────────────────────────────────
  no projects yet — create one with 'tempo projects add <slug>'
`

const goldenEntriesEmpty = `
Entries
────────────────────────────────────────────────────────────────────────────────
  no entries yet — start one with 'tempo start <project>'
`

const goldenEntriesNarrow = `
Entries                                3 entries
────────────────────────────────────────────────
  17 Aug  09:00–12:30  tempo   event s…   3h 30m
  17 Aug  13:00–14:00  café    espress…   1h 00m
▶ 17 Aug  14:05–now    tempo   writing…   1h 12m
`

const goldenReportUnicode = `
10 Aug 2026 – 14 Aug 2026                                                10h 00m
────────────────────────────────────────────────────────────────────────────────
By project
  café          ████████████████████████▌░░░░░░░░░░░░░░░░░░░░░░░░   50%   5h 00m
  café au lait  ██████████████▊░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   30%   3h 00m
  admin         █████████▊░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   20%   2h 00m
`
