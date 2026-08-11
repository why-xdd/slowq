package pgstat

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sample() Snapshot {
	return Snapshot{
		TakenAt: time.Now(),
		Statements: []Statement{
			// Cheap per call, but called constantly: the real cost centre.
			{QueryID: 1, Query: "SELECT 1", Calls: 1_000_000,
				TotalMS: 500_000, MeanMS: 0.5, MaxMS: 12, Rows: 1_000_000},
			// Slow per call, but rare: looks alarming, costs little.
			{QueryID: 2, Query: "SELECT * FROM report", Calls: 10,
				TotalMS: 40_000, MeanMS: 4000, MaxMS: 9000, Rows: 500_000},
			{QueryID: 3, Query: "UPDATE users SET seen = now()", Calls: 5000,
				TotalMS: 100_000, MeanMS: 20, MaxMS: 300, StddevMS: 60, Rows: 5000},
		},
	}
}

func TestRankByTotalPrefersAggregateCost(t *testing.T) {
	// The point of ranking by total: the query to fix is rarely the slowest.
	ranked := sample().Rank(ByTotal, 3)
	if ranked[0].QueryID != 1 {
		t.Errorf("expected the frequently-called query first, got id %d", ranked[0].QueryID)
	}
}

func TestRankByMeanPrefersPerCallCost(t *testing.T) {
	ranked := sample().Rank(ByMean, 3)
	if ranked[0].QueryID != 2 {
		t.Errorf("expected the slowest-per-call query first, got id %d", ranked[0].QueryID)
	}
}

func TestRankByVarianceWeightsByTotalTime(t *testing.T) {
	ranked := sample().Rank(ByVariance, 3)
	if ranked[0].QueryID != 3 {
		t.Errorf("expected the erratic query first, got id %d", ranked[0].QueryID)
	}
}

func TestRankLimitsAndDoesNotMutate(t *testing.T) {
	snapshot := sample()
	original := snapshot.Statements[0].QueryID

	if got := len(snapshot.Rank(ByMean, 2)); got != 2 {
		t.Errorf("limit not applied: got %d rows", got)
	}
	if snapshot.Statements[0].QueryID != original {
		t.Error("Rank must not reorder the caller's snapshot")
	}
}

func TestShareOfTotal(t *testing.T) {
	snapshot := sample()
	share := snapshot.ShareOfTotal(snapshot.Statements[0])
	if share < 0.7 || share > 0.8 {
		t.Errorf("share = %.3f, want ~0.78", share)
	}
}

func TestShareOfEmptySnapshotIsZero(t *testing.T) {
	if got := (Snapshot{}).ShareOfTotal(Statement{TotalMS: 5}); got != 0 {
		t.Errorf("share of an empty snapshot = %v, want 0", got)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := Save(sample(), path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := FromJSON(path)
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if len(loaded.Statements) != 3 {
		t.Fatalf("got %d statements, want 3", len(loaded.Statements))
	}
	if loaded.Statements[0].TotalMS != 500_000 {
		t.Errorf("TotalMS did not survive the round trip: %v", loaded.Statements[0].TotalMS)
	}
}

func TestFromJSONAcceptsABareArray(t *testing.T) {
	// Hand-rolled exports and psql dumps produce a bare array rather than the
	// wrapped object; rejecting those would make the file mode useless in
	// exactly the situation it exists for.
	path := filepath.Join(t.TempDir(), "bare.json")
	body := `[{"queryid":9,"query":"SELECT 1","calls":3,"total_exec_time":30}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := FromJSON(path)
	if err != nil {
		t.Fatalf("FromJSON on a bare array: %v", err)
	}
	if len(loaded.Statements) != 1 || loaded.Statements[0].QueryID != 9 {
		t.Errorf("bare array not parsed: %+v", loaded.Statements)
	}
}

func TestFromJSONReportsAMissingFileClearly(t *testing.T) {
	if _, err := FromJSON(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

// -- diffing ----------------------------------------------------------------

func TestDiffReportsOnlyTheWindow(t *testing.T) {
	before := Snapshot{Statements: []Statement{
		{QueryID: 1, Query: "SELECT 1", Calls: 100, TotalMS: 1000, Rows: 100},
	}}
	after := Snapshot{Statements: []Statement{
		{QueryID: 1, Query: "SELECT 1", Calls: 150, TotalMS: 4000, Rows: 150},
	}}

	delta := Diff(before, after).Statements
	if len(delta) != 1 {
		t.Fatalf("got %d statements, want 1", len(delta))
	}

	if delta[0].Calls != 50 {
		t.Errorf("calls = %d, want 50", delta[0].Calls)
	}
	if delta[0].TotalMS != 3000 {
		t.Errorf("total = %v, want 3000", delta[0].TotalMS)
	}
	// 3000 ms over 50 calls: the window's mean is 60 ms, not the lifetime 26.7.
	if delta[0].MeanMS != 60 {
		t.Errorf("mean = %v, want 60 (the window's mean, not the lifetime one)",
			delta[0].MeanMS)
	}
}

func TestDiffDropsStatementsThatDidNotRun(t *testing.T) {
	before := Snapshot{Statements: []Statement{
		{QueryID: 1, Calls: 100, TotalMS: 1000},
		{QueryID: 2, Calls: 50, TotalMS: 500},
	}}
	after := Snapshot{Statements: []Statement{
		{QueryID: 1, Calls: 100, TotalMS: 1000}, // unchanged
		{QueryID: 2, Calls: 80, TotalMS: 900},
	}}

	delta := Diff(before, after).Statements
	if len(delta) != 1 || delta[0].QueryID != 2 {
		t.Errorf("only statements that ran in the window belong in the delta: %+v", delta)
	}
}

func TestDiffKeepsStatementsFirstSeenInTheWindow(t *testing.T) {
	before := Snapshot{Statements: []Statement{{QueryID: 1, Calls: 10, TotalMS: 100}}}
	after := Snapshot{Statements: []Statement{
		{QueryID: 1, Calls: 10, TotalMS: 100},
		{QueryID: 2, Calls: 7, TotalMS: 700}, // brand new — often the regression
	}}

	delta := Diff(before, after).Statements
	if len(delta) != 1 || delta[0].QueryID != 2 {
		t.Errorf("a newly-appearing statement must survive the diff: %+v", delta)
	}
}

func TestDiffHandlesCounterReset(t *testing.T) {
	// pg_stat_statements_reset() or a restart makes the later snapshot smaller.
	// Reporting negative durations would be worse than reporting nothing.
	before := Snapshot{Statements: []Statement{{QueryID: 1, Calls: 500, TotalMS: 5000}}}
	after := Snapshot{Statements: []Statement{{QueryID: 1, Calls: 10, TotalMS: 100}}}

	for _, s := range Diff(before, after).Statements {
		if s.Calls < 0 || s.TotalMS < 0 {
			t.Errorf("negative counters after a reset: %+v", s)
		}
	}
}
