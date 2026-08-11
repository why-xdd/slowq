// Package pgstat models what pg_stat_statements records and the arithmetic
// worth doing on it.
package pgstat

import (
	"sort"
	"time"
)

// Statement is one row of pg_stat_statements, reduced to the columns that
// actually inform a decision.
type Statement struct {
	QueryID    int64   `json:"queryid"`
	Query      string  `json:"query"`
	Calls      int64   `json:"calls"`
	TotalMS    float64 `json:"total_exec_time"`
	MinMS      float64 `json:"min_exec_time"`
	MaxMS      float64 `json:"max_exec_time"`
	MeanMS     float64 `json:"mean_exec_time"`
	StddevMS   float64 `json:"stddev_exec_time"`
	Rows       int64   `json:"rows"`
	SharedHit  int64   `json:"shared_blks_hit"`
	SharedRead int64   `json:"shared_blks_read"`
	TempRead   int64   `json:"temp_blks_read"`
	TempWrite  int64   `json:"temp_blks_written"`
	Database   string  `json:"database,omitempty"`
	User       string  `json:"user,omitempty"`
}

// Total returns the statement's total execution time as a duration.
func (s Statement) Total() time.Duration {
	return time.Duration(s.TotalMS * float64(time.Millisecond))
}

// RowsPerCall is how much work each call returns. A query averaging tens of
// thousands of rows per call is usually a missing LIMIT or a join that
// multiplied, not a query that genuinely needs the data.
func (s Statement) RowsPerCall() float64 {
	if s.Calls == 0 {
		return 0
	}
	return float64(s.Rows) / float64(s.Calls)
}

// CacheHitRatio is the share of block reads served from shared buffers.
//
// A low ratio on a hot query means Postgres is going to disk for pages it
// should be holding — either the working set outgrew shared_buffers, or a
// sequential scan is evicting everything else on every call.
func (s Statement) CacheHitRatio() float64 {
	total := s.SharedHit + s.SharedRead
	if total == 0 {
		return 1
	}
	return float64(s.SharedHit) / float64(total)
}

// SpillsToDisk reports whether the statement used temporary files.
//
// Temp files mean a sort or hash did not fit in work_mem. It is one of the
// few signals in this view that points at a specific, actionable setting.
func (s Statement) SpillsToDisk() bool {
	return s.TempRead > 0 || s.TempWrite > 0
}

// Variability is the coefficient of variation of execution time.
//
// It separates "slow" from "unpredictable". A query at a steady 200 ms is a
// capacity question; one averaging 200 ms with a standard deviation of 900 ms
// is a correctness or locking question, and users experience it as the service
// randomly hanging.
func (s Statement) Variability() float64 {
	if s.MeanMS == 0 {
		return 0
	}
	return s.StddevMS / s.MeanMS
}

// Snapshot is a set of statements read at one moment.
type Snapshot struct {
	Statements []Statement `json:"statements"`
	TakenAt    time.Time   `json:"taken_at"`
	Version    string      `json:"version,omitempty"`
}

// TotalTime sums execution time across every statement.
func (s Snapshot) TotalTime() float64 {
	var total float64
	for _, statement := range s.Statements {
		total += statement.TotalMS
	}
	return total
}

// SortBy is a ranking criterion.
type SortBy string

const (
	// ByTotal is the default, and usually the right one. The query to fix is
	// rarely the slowest — it is the one whose per-call cost times its call
	// count dominates. A 5 ms query called two million times costs far more
	// than a 4-second report that runs nightly.
	ByTotal SortBy = "total"
	ByMean  SortBy = "mean"
	ByCalls SortBy = "calls"
	ByRows  SortBy = "rows"
	// ByVariance surfaces the queries users describe as "sometimes it just hangs".
	ByVariance SortBy = "variance"
)

// Rank returns statements ordered by the given criterion, highest first.
func (s Snapshot) Rank(by SortBy, limit int) []Statement {
	ranked := make([]Statement, len(s.Statements))
	copy(ranked, s.Statements)

	key := func(st Statement) float64 {
		switch by {
		case ByMean:
			return st.MeanMS
		case ByCalls:
			return float64(st.Calls)
		case ByRows:
			return st.RowsPerCall()
		case ByVariance:
			// Weighted by total time so a wildly variable query that runs twice
			// a day does not outrank one that hangs on the checkout path.
			return st.Variability() * st.TotalMS
		default:
			return st.TotalMS
		}
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		return key(ranked[i]) > key(ranked[j])
	})

	if limit > 0 && limit < len(ranked) {
		ranked = ranked[:limit]
	}
	return ranked
}

// ShareOfTotal is what fraction of all execution time a statement accounts for.
func (s Snapshot) ShareOfTotal(statement Statement) float64 {
	total := s.TotalTime()
	if total == 0 {
		return 0
	}
	return statement.TotalMS / total
}
