// Command slowq ranks Postgres statements by the time they actually cost and
// says what is likely wrong with the worst of them.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/why-xdd/slowq/internal/analyze"
	"github.com/why-xdd/slowq/internal/pgstat"
	"github.com/why-xdd/slowq/internal/render"
)

var version = "0.1.0"

const usage = `slowq - find the Postgres queries that actually cost you time

Usage:
  slowq [flags]

Sources (one of):
  -dsn string     Postgres connection string, or $DATABASE_URL
  -file string    JSON snapshot written by -save, or "-" for stdin

Examples:
  slowq -dsn "postgres://localhost/app"
  slowq -dsn "$DATABASE_URL" -save before.json
  slowq -file before.json -diff after.json     # what changed in the window
  slowq -file snapshot.json -sort variance     # what hangs unpredictably
  slowq -file snapshot.json -json | jq '.[0]'

Flags:
`

func main() {
	var (
		dsn     = flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres connection string")
		file    = flag.String("file", "", "read a JSON snapshot instead of a database")
		diff    = flag.String("diff", "", "second snapshot; report the delta between the two")
		save    = flag.String("save", "", "write the snapshot to this path")
		limit   = flag.Int("limit", 20, "statements to show")
		fetch   = flag.Int("fetch", 200, "statements to read from the server")
		sortBy  = flag.String("sort", "total", "total | mean | calls | rows | variance")
		width   = flag.Int("width", 100, "output width")
		verbose = flag.Bool("verbose", false, "print full queries, re-indented")
		asJSON  = flag.Bool("json", false, "machine-readable output")
		colour  = flag.Bool("color", false, "force colour even when redirected")
		slowMS  = flag.Float64("slow-ms", 50, "mean duration considered slow")
		showVer = flag.Bool("version", false, "print version and exit")
	)

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVer {
		fmt.Println("slowq", version)
		return
	}

	if err := run(config{
		dsn: *dsn, file: *file, diff: *diff, save: *save,
		limit: *limit, fetch: *fetch, sortBy: *sortBy, width: *width,
		verbose: *verbose, asJSON: *asJSON, colour: *colour, slowMS: *slowMS,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "slowq: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	dsn, file, diff, save string
	limit, fetch, width   int
	sortBy                string
	verbose, asJSON       bool
	colour                bool
	slowMS                float64
}

func run(cfg config) error {
	snapshot, err := load(cfg.dsn, cfg.file, cfg.fetch)
	if err != nil {
		return err
	}

	if cfg.save != "" {
		if err := pgstat.Save(snapshot, cfg.save); err != nil {
			return fmt.Errorf("save snapshot: %w", err)
		}
		fmt.Fprintf(os.Stderr, "snapshot written to %s\n", cfg.save)
	}

	if cfg.diff != "" {
		later, err := pgstat.FromJSON(cfg.diff)
		if err != nil {
			return err
		}
		snapshot = pgstat.Diff(snapshot, later)
	}

	if len(snapshot.Statements) == 0 {
		return fmt.Errorf("no statements found (empty snapshot, or nothing ran in the window)")
	}

	thresholds := analyze.DefaultThresholds()
	thresholds.SlowMeanMS = cfg.slowMS

	ranked := snapshot.Rank(pgstat.SortBy(cfg.sortBy), cfg.limit)

	if cfg.asJSON {
		return emitJSON(snapshot, ranked, thresholds)
	}

	printer := render.New(os.Stdout, cfg.colour)
	printer.Summary(snapshot, len(ranked))

	var critical, warnings int
	for i, statement := range ranked {
		share := snapshot.ShareOfTotal(statement)
		findings := analyze.Analyze(statement, share, thresholds)

		for _, f := range findings {
			switch f.Severity {
			case analyze.Critical:
				critical++
			case analyze.Warning:
				warnings++
			}
		}

		printer.Statement(i+1, statement, share, findings,
			analyze.SuggestIndex(statement.Query), cfg.width, cfg.verbose)
	}

	printer.Footer(critical, warnings)
	return nil
}

func load(dsn, file string, fetch int) (pgstat.Snapshot, error) {
	if file != "" {
		return pgstat.FromJSON(file)
	}
	if dsn == "" {
		return pgstat.Snapshot{}, fmt.Errorf(
			"need -dsn or -file (or set DATABASE_URL); see -h")
	}

	// A bounded timeout rather than none: this gets run during incidents, and
	// a command that hangs against an unreachable database is worse than one
	// that fails quickly and says so.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return pgstat.FromPostgres(ctx, dsn, fetch)
}

type report struct {
	Rank        int                      `json:"rank"`
	Fingerprint string                   `json:"fingerprint"`
	Query       string                   `json:"query"`
	Command     string                   `json:"command"`
	Calls       int64                    `json:"calls"`
	TotalMS     float64                  `json:"total_ms"`
	MeanMS      float64                  `json:"mean_ms"`
	MaxMS       float64                  `json:"max_ms"`
	RowsPerCall float64                  `json:"rows_per_call"`
	Share       float64                  `json:"share_of_total"`
	Findings    []analyze.Finding        `json:"findings"`
	Index       *analyze.IndexSuggestion `json:"index_suggestion,omitempty"`
}

func emitJSON(
	snapshot pgstat.Snapshot, ranked []pgstat.Statement, thresholds analyze.Thresholds,
) error {
	reports := make([]report, 0, len(ranked))
	for i, s := range ranked {
		share := snapshot.ShareOfTotal(s)
		reports = append(reports, report{
			Rank:        i + 1,
			Fingerprint: analyze.Fingerprint(s.Query),
			Query:       analyze.Normalize(s.Query),
			Command:     analyze.Command(s.Query),
			Calls:       s.Calls,
			TotalMS:     s.TotalMS,
			MeanMS:      s.MeanMS,
			MaxMS:       s.MaxMS,
			RowsPerCall: s.RowsPerCall(),
			Share:       share,
			Findings:    analyze.Analyze(s, share, thresholds),
			Index:       analyze.SuggestIndex(s.Query),
		})
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(reports)
}
