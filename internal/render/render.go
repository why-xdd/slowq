// Package render writes the report to a terminal.
package render

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/why-xdd/slowq/internal/analyze"
	"github.com/why-xdd/slowq/internal/pgstat"
)

// ANSI codes, written out rather than pulled from a styling library. The
// output is a table and a few coloured labels; a dependency tree for that is
// not a trade worth making in a tool people install to debug an incident.
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	yellow = "\033[33m"
	green  = "\033[32m"
	cyan   = "\033[36m"
	grey   = "\033[90m"
)

// Printer writes a report, with colour only when the destination can show it.
type Printer struct {
	out    io.Writer
	colour bool
}

// New returns a Printer. Colour is disabled when output is redirected, because
// escape codes in a file someone pastes into a ticket help nobody.
func New(out io.Writer, forceColour bool) *Printer {
	colour := forceColour
	if !forceColour {
		if file, ok := out.(*os.File); ok {
			info, err := file.Stat()
			colour = err == nil && (info.Mode()&os.ModeCharDevice) != 0
			if os.Getenv("NO_COLOR") != "" {
				colour = false
			}
		}
	}
	return &Printer{out: out, colour: colour}
}

func (p *Printer) style(code, s string) string {
	if !p.colour {
		return s
	}
	return code + s + reset
}

func (p *Printer) printf(format string, args ...any) {
	fmt.Fprintf(p.out, format, args...)
}

func severityStyle(s analyze.Severity) string {
	switch s {
	case analyze.Critical:
		return red
	case analyze.Warning:
		return yellow
	default:
		return grey
	}
}

// Summary prints the header block.
func (p *Printer) Summary(snapshot pgstat.Snapshot, shown int) {
	var calls int64
	for _, s := range snapshot.Statements {
		calls += s.Calls
	}

	total := snapshot.TotalTime()
	p.printf("\n%s\n", p.style(bold, "slowq"))
	p.printf("%s\n\n", p.style(dim, fmt.Sprintf(
		"%d statements · %s total execution time · %s calls · showing top %d",
		len(snapshot.Statements), formatMS(total), formatCount(calls), shown)))
}

// Statement prints one ranked entry with its findings.
func (p *Printer) Statement(
	position int, s pgstat.Statement, share float64, findings []analyze.Finding,
	suggestion *analyze.IndexSuggestion, width int, verbose bool,
) {
	worst := analyze.Worst(findings)
	marker := p.style(severityStyle(worst), "●")

	p.printf("%s %s %s\n",
		marker,
		p.style(bold, fmt.Sprintf("#%d", position)),
		p.style(cyan, analyze.Fingerprint(s.Query)))

	p.printf("  %s\n", p.style(dim, fmt.Sprintf(
		"%s total (%.1f%%) · %s calls · %s mean · %s max · %.0f rows/call",
		formatMS(s.TotalMS), share*100, formatCount(s.Calls),
		formatMS(s.MeanMS), formatMS(s.MaxMS), s.RowsPerCall())))

	query := analyze.Normalize(s.Query)
	if verbose {
		for _, line := range strings.Split(analyze.Pretty(query), "\n") {
			p.printf("  %s\n", p.style(green, "  "+line))
		}
	} else {
		p.printf("  %s\n", p.style(green, analyze.Truncate(query, width)))
	}

	for _, f := range findings {
		p.printf("    %s %s\n",
			p.style(severityStyle(f.Severity), symbolFor(f.Severity)),
			p.style(bold, f.What))
		if f.Why != "" {
			for _, line := range wrap(f.Why, width-6) {
				p.printf("      %s\n", p.style(dim, line))
			}
		}
		if f.Suggest != "" {
			for _, line := range wrap(f.Suggest, width-6) {
				p.printf("      %s\n", p.style(cyan, line))
			}
		}
	}

	if suggestion != nil {
		p.printf("    %s %s\n", p.style(cyan, "▸"), p.style(bold, "index candidate"))
		p.printf("      %s\n", p.style(cyan, suggestion.DDL))
		for _, line := range wrap(suggestion.Rationale, width-6) {
			p.printf("      %s\n", p.style(dim, line))
		}
	}

	p.printf("\n")
}

// Footer prints the closing note.
func (p *Printer) Footer(critical, warnings int) {
	parts := []string{}
	if critical > 0 {
		parts = append(parts, p.style(red, fmt.Sprintf("%d critical", critical)))
	}
	if warnings > 0 {
		parts = append(parts, p.style(yellow, fmt.Sprintf("%d warnings", warnings)))
	}
	if len(parts) == 0 {
		p.printf("%s\n\n", p.style(green, "Nothing alarming in this snapshot."))
		return
	}

	p.printf("%s\n", strings.Join(parts, p.style(dim, " · ")))
	p.printf("%s\n\n", p.style(dim,
		"These are heuristics over query text. Confirm with EXPLAIN (ANALYZE, BUFFERS) "+
			"before changing anything."))
}

func symbolFor(s analyze.Severity) string {
	switch s {
	case analyze.Critical:
		return "✗"
	case analyze.Warning:
		return "!"
	default:
		return "·"
	}
}

func formatMS(ms float64) string {
	switch {
	case ms >= 3_600_000:
		return fmt.Sprintf("%.1fh", ms/3_600_000)
	case ms >= 60_000:
		return fmt.Sprintf("%.1fm", ms/60_000)
	case ms >= 1_000:
		return fmt.Sprintf("%.1fs", ms/1_000)
	case ms >= 1:
		return fmt.Sprintf("%.0fms", ms)
	default:
		return fmt.Sprintf("%.2fms", ms)
	}
}

func formatCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

// wrap breaks text to a width without splitting words.
func wrap(text string, width int) []string {
	if width < 20 {
		width = 20
	}

	var lines []string
	var line strings.Builder

	for _, word := range strings.Fields(text) {
		if line.Len() > 0 && line.Len()+1+len(word) > width {
			lines = append(lines, line.String())
			line.Reset()
		}
		if line.Len() > 0 {
			line.WriteByte(' ')
		}
		line.WriteString(word)
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return lines
}
