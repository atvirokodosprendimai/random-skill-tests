package render

import (
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxSaneDuration bounds what Duration is willing to print. A time tracker
// reporting a single interval measured in years is describing a corrupt log, not
// a long day, and "87600h 00m" hides that behind a plausible-looking number
// where an em dash does not.
const maxSaneDuration = 10000 * time.Hour

// Duration formats d as a compact human duration, e.g. "2h 05m".
//
// Zero is "0m", anything under an hour is bare minutes ("45m"), and an hour or
// more is "2h 05m" with the minutes zero-padded so the column stays aligned.
// A negative or absurd d renders as "—": those are bugs upstream, and a
// renderer's job is to make them visible, not to guess at a plausible number.
//
// Seconds are truncated rather than rounded, so the printed total never exceeds
// the time actually tracked.
func Duration(d time.Duration) string {
	if d < 0 || d >= maxSaneDuration {
		return "—"
	}
	mins := int64(d / time.Minute)
	switch {
	case mins == 0:
		return "0m"
	case mins < 60:
		return strconv.FormatInt(mins, 10) + "m"
	default:
		h := strconv.FormatInt(mins/60, 10)
		m := strconv.FormatInt(mins%60, 10)
		if len(m) == 1 {
			m = "0" + m
		}
		return h + "h " + m + "m"
	}
}

// currencySymbols holds the symbols worth printing. Everything outside it falls
// back to the ISO code, which is always correct and never ambiguous — better a
// reader sees "JPY 1,200.00" than a symbol this package guessed wrong.
var currencySymbols = map[string]string{
	"EUR": "€",
	"USD": "$",
	"GBP": "£",
}

// Money formats integer cents in currency, e.g. "€1,234.50" or "USD 1,234.50".
//
// The amount is always two decimal places with thousands separators. EUR, USD
// and GBP print as a symbol prefix; any other code prints as the code plus a
// space. A negative amount carries its sign in front of the symbol ("-€5.00")
// because that is where a reader scanning a column of figures looks for it.
//
// The formatting is done on the integer throughout: binary floating point cannot
// represent 0.10, and an invoice that is a cent off is far worse than one that
// is ugly.
func Money(cents int64, currency string) string {
	// Take the magnitude in unsigned arithmetic: negating math.MinInt64
	// overflows int64, and an invoice is not the place to wrap around.
	neg := cents < 0
	mag := uint64(cents)
	if neg {
		mag = uint64(-(cents + 1)) + 1
	}

	frac := strconv.FormatUint(mag%100, 10)
	if len(frac) == 1 {
		frac = "0" + frac
	}
	digits := group(mag/100) + "." + frac
	sign := ""
	if neg {
		sign = "-"
	}

	code := strings.ToUpper(strings.TrimSpace(currency))
	switch sym, known := currencySymbols[code]; {
	case known:
		return sign + sym + digits
	case code == "":
		return sign + digits
	default:
		return code + " " + sign + digits
	}
}

// group inserts thousands separators into n's decimal digits.
func group(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// ellipsis marks a truncated cell. One rune, so the cut costs one column.
const ellipsis = "…"

// truncate shortens s to at most n columns, marking the cut with an ellipsis.
//
// Truncating rather than wrapping is a deliberate table-wide rule: a wrapped
// cell shifts every column to its right and destroys the alignment that makes
// the table readable in the first place.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return ellipsis
	}
	return string([]rune(s)[:n-1]) + ellipsis
}

// padRight left-aligns s in an n-column cell, truncating if it does not fit.
// Used for text — names, notes, labels — where losing the tail is survivable.
func padRight(s string, n int) string {
	s = truncate(s, n)
	if d := n - utf8.RuneCountInString(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// padLeft right-aligns s in an n-column cell.
//
// Unlike padRight it never truncates: it is used for durations and money, and a
// shortened number is not a shortened label — it is a wrong number. An oversized
// value pushes its column instead, which is visible and therefore fixable.
func padLeft(s string, n int) string {
	if d := n - utf8.RuneCountInString(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// barBlocks are the left-anchored partial block runes, one eighth through seven
// eighths of a cell.
var barBlocks = [...]string{"▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// bar draws share (0..1) as a proportional bar exactly width columns wide.
//
// It resolves to the nearest eighth of a cell rather than to whole cells, so two
// shares a percentage point apart do not collapse onto the same glyph — at 40
// columns a whole-cell bar can only express 2.5% steps, which turns a breakdown
// into a staircase. The unfilled remainder is drawn as light shade rather than
// left blank so the bar's full extent, and therefore the scale being compared
// against, stays visible on every row.
func bar(share float64, width int) string {
	if width <= 0 {
		return ""
	}
	share = clamp01(share)

	eighths := int(math.Round(share * float64(width) * 8))
	full := eighths / 8
	if full >= width {
		return strings.Repeat("█", width)
	}
	part := eighths % 8

	var b strings.Builder
	b.WriteString(strings.Repeat("█", full))
	rest := width - full
	if part > 0 {
		b.WriteString(barBlocks[part-1])
		rest--
	}
	b.WriteString(strings.Repeat("░", rest))
	return b.String()
}

// clamp01 pins a share into 0..1, treating NaN as zero. Shares arrive
// precomputed from the read models; the renderer defends the bar's width
// invariant anyway, because a bar that overruns its cell corrupts the whole
// table and a silently clamped one does not.
func clamp01(f float64) float64 {
	switch {
	case math.IsNaN(f) || f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// percent renders a share as a right-aligned whole-percent cell, e.g. " 84%".
func percent(share float64) string {
	return padLeft(strconv.Itoa(int(math.Round(clamp01(share)*100)))+"%", pctWidth)
}

// plural returns "1 entry" / "3 entries" style counts for header figures.
func plural(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}
