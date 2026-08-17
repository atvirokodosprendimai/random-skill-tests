package render

import (
	"bytes"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/random-skill-tests/internal/core"
)

// Fixtures build their times in time.Local so that the goldens below are stable
// in any zone: the renderer formats through Time.Local (the log is stored in
// UTC and localising is render's job per internal/core), and Local() on a
// time already in time.Local is the identity.
func at(hh, mm int) time.Time { return time.Date(2026, 8, 17, hh, mm, 0, 0, time.Local) }
func on(day int) time.Time    { return time.Date(2026, 8, day, 0, 0, 0, 0, time.Local) }

func mins(m int) time.Duration { return time.Duration(m) * time.Minute }

func sampleDay() core.DayView {
	running := core.EntryView{
		ID: "c3", Project: "tempo", Note: "writing the renderer",
		Tags: []string{"deep"}, Start: at(14, 5), Duration: mins(72), Running: true,
	}
	entries := []core.EntryView{
		{ID: "a1", Project: "tempo", Note: "event store replay", Tags: []string{"deep", "oss"},
			Start: at(9, 0), End: at(12, 30), Duration: mins(210)},
		{ID: "b2", Project: "admin", Tags: []string{"ops"},
			Start: at(13, 0), End: at(14, 0), Duration: mins(60)},
		running,
	}
	return core.DayView{
		Date:    on(17),
		Total:   mins(342),
		Running: &running,
		Entries: entries,
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

// render runs one renderer against a fresh buffer with colour disabled.
func render(t *testing.T, opt Options, fn func(*Renderer) error) string {
	t.Helper()
	var buf bytes.Buffer
	if err := fn(New(&buf, opt)); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestDump(t *testing.T) {
	t.Log("\n--- DAY ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Day(sampleDay()) }))
	t.Log("\n--- REPORT ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Report(sampleReport()) }))
	t.Log("\n--- INVOICE ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Invoice(sampleInvoice()) }))
	t.Log("\n--- PROJECTS ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Projects(sampleProjects()) }))
	t.Log("\n--- ENTRIES ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Entries(sampleEntries()) }))
	t.Log("\n--- NARROW DAY (48) ---\n" + render(t, Options{Width: 48}, func(r *Renderer) error { return r.Day(sampleDay()) }))
	t.Log("\n--- EMPTY DAY ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Day(core.DayView{Date: on(17)}) }))
	t.Log("\n--- EMPTY REPORT ---\n" + render(t, Options{}, func(r *Renderer) error {
		return r.Report(core.RangeReport{From: on(10), To: on(14)})
	}))
	t.Log("\n--- EMPTY INVOICE ---\n" + render(t, Options{}, func(r *Renderer) error {
		return r.Invoice(core.Invoice{From: on(1), To: on(31)})
	}))
	t.Log("\n--- EMPTY PROJECTS ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Projects(nil) }))
	t.Log("\n--- EMPTY ENTRIES ---\n" + render(t, Options{}, func(r *Renderer) error { return r.Entries(nil) }))
}
