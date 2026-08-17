package render

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0m"},
		{"sub-minute truncates down", 45 * time.Second, "0m"},
		{"one minute", time.Minute, "1m"},
		{"under an hour", 45 * time.Minute, "45m"},
		{"exactly an hour", time.Hour, "1h 00m"},
		{"pads the minutes", 2*time.Hour + 5*time.Minute, "2h 05m"},
		{"drops the seconds", 2*time.Hour + 5*time.Minute + 59*time.Second, "2h 05m"},
		{"over a day", 25 * time.Hour, "25h 00m"},
		{"negative", -time.Minute, "—"},
		{"absurd", maxSaneDuration, "—"},
		{"just under absurd", maxSaneDuration - time.Minute, "9999h 59m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Duration(tc.in); got != tc.want {
				t.Errorf("Duration(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMoney(t *testing.T) {
	tests := []struct {
		name     string
		cents    int64
		currency string
		want     string
	}{
		{"zero", 0, "EUR", "€0.00"},
		{"under a euro", 5, "EUR", "€0.05"},
		{"thousands separators", 1234550, "EUR", "€12,345.50"},
		{"millions", 123456789, "USD", "$1,234,567.89"},
		{"exactly one thousand", 100000, "GBP", "£1,000.00"},
		{"no separator under a thousand", 99999, "USD", "$999.99"},
		{"negative signs the symbol", -1234550, "EUR", "-€12,345.50"},
		{"unknown code falls back", 1234550, "JPY", "JPY 12,345.50"},
		{"unknown code negative", -1200, "JPY", "JPY -12.00"},
		{"lowercase code is normalised", 500, "usd", "$5.00"},
		{"empty currency is bare", 500, "", "5.00"},
		{"most negative int64 does not overflow", math.MinInt64, "USD", "-$92,233,720,368,547,758.08"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Money(tc.cents, tc.currency); got != tc.want {
				t.Errorf("Money(%d, %q) = %q, want %q", tc.cents, tc.currency, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"fits", "tempo", 8, "tempo"},
		{"exact fit is untouched", "tempo", 5, "tempo"},
		{"cuts and marks", "event store replay", 8, "event s…"},
		{"counts runes not bytes", "café au lait", 5, "café…"},
		{"single column", "tempo", 1, "…"},
		{"zero columns", "tempo", 0, ""},
		{"negative columns", "tempo", -3, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); tc.n > 0 && n > tc.n {
				t.Errorf("truncate(%q, %d) produced %d columns", tc.in, tc.n, n)
			}
		})
	}
}

func TestPadCountsRunes(t *testing.T) {
	// "café" is 5 bytes and 4 runes; a byte-padded cell would be one short.
	if got := padRight("café", 6); got != "café  " {
		t.Errorf("padRight = %q, want %q", got, "café  ")
	}
	if n := utf8.RuneCountInString(padRight("café", 6)); n != 6 {
		t.Errorf("padRight produced %d columns, want 6", n)
	}
	if got := padLeft("2h 05m", 8); got != "  2h 05m" {
		t.Errorf("padLeft = %q, want %q", got, "  2h 05m")
	}
	// padLeft must never shorten a figure: a truncated number is a wrong number.
	if got := padLeft("€1,234,567.89", 6); got != "€1,234,567.89" {
		t.Errorf("padLeft truncated a figure: %q", got)
	}
}

func TestBar(t *testing.T) {
	tests := []struct {
		name  string
		share float64
		width int
		want  string
	}{
		{"empty", 0, 8, "░░░░░░░░"},
		{"full", 1, 8, "████████"},
		{"half", 0.5, 8, "████░░░░"},
		{"partial eighth", 0.5625, 8, "████▌░░░"}, // 4.5 cells
		{"rounds into a partial cell", 0.05, 8, "▍░░░░░░░"},
		{"clamps above one", 1.5, 8, "████████"},
		{"clamps below zero", -0.5, 8, "░░░░░░░░"},
		{"NaN is empty", math.NaN(), 8, "░░░░░░░░"},
		{"zero width", 0.5, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := bar(tc.share, tc.width)
			if got != tc.want {
				t.Errorf("bar(%v, %d) = %q, want %q", tc.share, tc.width, got, tc.want)
			}
		})
	}
}

// TestBarWidthIsInvariant is the property the whole table depends on: whatever
// the share, a bar occupies exactly the cells it was given, or the columns to
// its right walk.
func TestBarWidthIsInvariant(t *testing.T) {
	for _, width := range []int{1, 7, 8, 23, 54} {
		for i := 0; i <= 1000; i++ {
			share := float64(i) / 1000
			if n := utf8.RuneCountInString(bar(share, width)); n != width {
				t.Fatalf("bar(%v, %d) is %d columns", share, width, n)
			}
		}
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		share float64
		want  string
	}{
		{0, "  0%"},
		{0.004, "  0%"},
		{0.185, " 19%"},
		{0.824, " 82%"},
		{1, "100%"},
		{1.4, "100%"},
	}
	for _, tc := range tests {
		if got := percent(tc.share); got != tc.want {
			t.Errorf("percent(%v) = %q, want %q", tc.share, got, tc.want)
		}
		if n := utf8.RuneCountInString(percent(tc.share)); n != pctWidth {
			t.Errorf("percent(%v) is %d columns, want %d", tc.share, n, pctWidth)
		}
	}
}

func TestGroup(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{999999, "999,999"},
		{1000000, "1,000,000"},
	}
	for _, tc := range tests {
		if got := group(tc.in); got != tc.want {
			t.Errorf("group(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "entry", "entries"); got != "1 entry" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(0, "entry", "entries"); got != "0 entries" {
		t.Errorf("plural(0) = %q", got)
	}
	if got := plural(3, "entry", "entries"); got != "3 entries" {
		t.Errorf("plural(3) = %q", got)
	}
}

// TestNewDefaultsWidth checks that a non-positive budget falls back to 80 rather
// than producing a zero-width rule.
func TestNewDefaultsWidth(t *testing.T) {
	for _, w := range []int{0, -1} {
		out := draw(t, Options{Width: w}, func(r *Renderer) error { return r.Projects(nil) })
		rule := strings.Split(out, "\n")[1]
		if n := utf8.RuneCountInString(rule); n != defaultWidth {
			t.Errorf("Width=%d produced a %d-column rule, want %d", w, n, defaultWidth)
		}
	}
}
