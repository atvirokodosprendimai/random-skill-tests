// Package render turns tempo's read models into terminal output.
//
// It is deliberately a pure function of its input: a Renderer never reads the
// clock, never touches the filesystem and never logs. Everything it needs —
// totals, shares, the elapsed time of a running entry — is already computed by
// the read models in internal/core, so one view struct always produces one exact
// sequence of bytes. That is what makes golden tests possible, and what keeps
// "how it looks" out of "what it means".
//
// # Layout
//
// All five renderers speak one visual language: a title line with the headline
// figure right-aligned to the width budget, a light box-drawing rule beneath it,
// then two-space-indented rows whose numeric columns are right-aligned to that
// same edge. Columns are computed from [Options].Width rather than hardcoded,
// and overlong text truncates with an ellipsis instead of wrapping, because a
// wrapped cell pushes every column after it out of alignment and a table you
// cannot scan is worse than a note you cannot finish reading.
//
// # Widths
//
// Padding and truncation count runes ([utf8.RuneCountInString]), not bytes, so a
// project called "café" does not shear its column the way a byte-counting
// %-20s would. Runes are not the whole truth: a combining accent counts twice
// where the terminal draws one cell, and an East-Asian ideograph counts once
// where the terminal draws two. Correcting either needs a grapheme-cluster or
// East-Asian-width table that the standard library does not ship, so rune
// counting is the honest limit of a stdlib-only renderer — right for the Latin
// and accented project names this tool actually sees, off by a cell for CJK.
//
// # Colour
//
// With [Options].Color false the output contains no escape byte at all, which is
// what makes it safe to redirect into a file and possible to compare in a test.
// Colour is never load-bearing: a single accent marks totals, bars and the
// running timer, dim marks secondary metadata, and every fact a colour hints at
// is also spelled out in text — so colour-blind readers, NO_COLOR users and log
// files lose nothing.
package render

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Options configures a Renderer.
type Options struct {
	Color bool // emit ANSI escapes
	Width int  // total column budget; <= 0 means 80
}

// defaultWidth is the column budget used when Options.Width is unset. Eighty is
// the width every terminal is at least as wide as, and the one a reader
// redirecting output into a file or a patch expects.
const defaultWidth = 80

// Renderer writes tempo's read models as terminal output.
//
// A Renderer is immutable after construction and holds no buffers, so it is safe
// to reuse for many views; it is not safe to share across goroutines only
// because the underlying io.Writer generally is not.
type Renderer struct {
	w     io.Writer
	color bool
	width int
}

// New returns a Renderer writing to w.
//
// A non-positive opt.Width is replaced by the 80-column default rather than
// rejected: a renderer is the wrong place to fail a command that has already
// done its work.
func New(w io.Writer, opt Options) *Renderer {
	width := opt.Width
	if width <= 0 {
		width = defaultWidth
	}
	return &Renderer{w: w, color: opt.Color, width: width}
}

// ANSI escapes for the whole palette. It is deliberately three entries long:
// an accent for the figures a reader came for, dim for the metadata around
// them, and bold for the title. Anything more and colour starts carrying
// meaning that the text does not.
const (
	ansiReset  = "\x1b[0m"
	ansiAccent = "\x1b[36m"
	ansiDim    = "\x1b[2m"
	ansiBold   = "\x1b[1m"
)

// paint wraps s in an escape pair, or returns it untouched when colour is off.
//
// Callers must pad and truncate *before* painting: escape bytes are zero cells
// wide on screen but very much non-zero to utf8.RuneCountInString, so colouring
// first and aligning second would shear every column it touched.
func (r *Renderer) paint(code, s string) string {
	if !r.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (r *Renderer) accent(s string) string { return r.paint(ansiAccent, s) }
func (r *Renderer) dim(s string) string    { return r.paint(ansiDim, s) }
func (r *Renderer) bold(s string) string   { return r.paint(ansiBold, s) }

// accentIf paints s only when on, so a caller can mark the running row without
// branching around every cell it builds.
func (r *Renderer) accentIf(on bool, s string) string {
	if !on {
		return s
	}
	return r.accent(s)
}

// errWriter accumulates the first write error and turns every later write into a
// no-op. It exists so a renderer can lay out a whole view as straight-line code
// and still report the failure exactly once, instead of interleaving an
// `if err != nil` with every line and losing the shape of the layout.
type errWriter struct {
	w   io.Writer
	err error

	// sinceRule counts lines written since the last rule, so section can tell
	// whether it is opening the body or breaking it up. See section.
	sinceRule int
}

func (e *errWriter) write(s string) {
	if e.err != nil {
		return
	}
	_, e.err = io.WriteString(e.w, s)
}

// line writes s followed by a newline.
func (e *errWriter) line(s string) {
	e.sinceRule++
	e.write(s + "\n")
}

// title writes a section header: the label on the left, the headline figure
// right-aligned to the width budget. Every renderer opens with one so the five
// commands read as one program rather than five.
func (r *Renderer) title(e *errWriter, left, right string) {
	if right == "" {
		e.line(r.bold(left))
		return
	}
	// Compute the gap on the unpainted strings; see paint.
	gap := r.width - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	if gap < 1 {
		gap = 1
	}
	e.line(r.bold(left) + strings.Repeat(" ", gap) + r.accent(right))
}

// rule writes the horizontal divider under a title. One light box-drawing rune
// across the budget, rather than ASCII +---+ noise, because the table's job is
// to disappear behind the numbers.
func (r *Renderer) rule(e *errWriter) {
	e.line(strings.Repeat("─", r.width))
	e.sinceRule = 0
}

// section writes the dim caption that opens a sub-table, preceded by a blank
// line so the eye can find the break without spending another rule on it.
//
// The blank is suppressed when the caption is the first thing under a rule: the
// rule has already made the break, and a blank line stacked on top of it reads
// as a missing table rather than as breathing room.
func (r *Renderer) section(e *errWriter, name string) {
	if e.sinceRule > 0 {
		e.line("")
	}
	e.line(r.dim(name))
}

// empty writes a designed empty state.
//
// Every empty state names the command that fills it. A user who ran the right
// command and got nothing back needs the next step, not a blank screen — this is
// the single most-noticed detail in a CLI, so none of the five renderers is
// allowed to fall through to a bare total or an empty table.
func (r *Renderer) empty(e *errWriter, msg string) {
	e.line("  " + r.dim(msg))
}
