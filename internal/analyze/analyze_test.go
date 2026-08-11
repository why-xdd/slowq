package analyze

import (
	"strings"
	"testing"

	"github.com/why-xdd/slowq/internal/pgstat"
)

func TestNormalizeCollapsesLiterals(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"string literals",
			"SELECT * FROM users WHERE email = 'a@b.com'",
			"SELECT * FROM users WHERE email = ?",
		},
		{
			"numbers",
			"SELECT * FROM orders WHERE total > 100.50 AND id = 7",
			"SELECT * FROM orders WHERE total > ? AND id = ?",
		},
		{
			"existing placeholders",
			"SELECT * FROM users WHERE id = $1",
			"SELECT * FROM users WHERE id = ?",
		},
		{
			"escaped quote inside a literal",
			"SELECT * FROM t WHERE name = 'O''Brien'",
			"SELECT * FROM t WHERE name = ?",
		},
		{
			"comments and whitespace",
			"SELECT id -- the pk\nFROM   users\n/* hint */ WHERE x = 1",
			"SELECT id FROM users WHERE x = ?",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.input); got != tc.want {
				t.Errorf("Normalize()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestInListsOfDifferentLengthsShareAFingerprint(t *testing.T) {
	// Without this, one problem shows up as a hundred separate rows and none of
	// them looks important enough to fix.
	short := "SELECT * FROM users WHERE id IN ($1, $2, $3)"
	long := "SELECT * FROM users WHERE id IN ($1, $2, $3, $4, $5, $6, $7, $8)"

	if Fingerprint(short) != Fingerprint(long) {
		t.Errorf("IN lists of different lengths fingerprinted differently:\n%s\n%s",
			Normalize(short), Normalize(long))
	}
}

func TestFingerprintIsStableAndDistinguishing(t *testing.T) {
	a := "SELECT * FROM users WHERE id = 1"
	b := "SELECT  *  FROM users WHERE id = 999"
	c := "SELECT * FROM orders WHERE id = 1"

	if Fingerprint(a) != Fingerprint(b) {
		t.Error("same query shape should fingerprint identically")
	}
	if Fingerprint(a) == Fingerprint(c) {
		t.Error("different tables should fingerprint differently")
	}
}

func TestCommandSkipsCTEPrefix(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":                          "SELECT",
		"  update users set x = 1":          "UPDATE",
		"WITH recent AS (...) SELECT * ...": "RECENT", // the CTE name, not a verb
		"":                                  "UNKNOWN",
	}
	for input, want := range cases {
		if got := Command(input); got != want {
			t.Errorf("Command(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTruncateKeepsWholeWords(t *testing.T) {
	got := Truncate("SELECT id, name, email FROM users WHERE active", 20)
	if len([]rune(got)) > 20 {
		t.Errorf("Truncate returned %d runes: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated text should be marked: %q", got)
	}
}

// -- statement arithmetic ---------------------------------------------------

func TestVariabilitySeparatesSlowFromErratic(t *testing.T) {
	steady := pgstat.Statement{MeanMS: 200, StddevMS: 10}
	erratic := pgstat.Statement{MeanMS: 200, StddevMS: 900}

	if steady.Variability() >= 1 {
		t.Errorf("steady query looked erratic: %.2f", steady.Variability())
	}
	if erratic.Variability() < 1 {
		t.Errorf("erratic query looked steady: %.2f", erratic.Variability())
	}
}

func TestCacheHitRatioWithNoReads(t *testing.T) {
	// No block access at all is not a 0% hit rate; reporting it as one would
	// flag every trivial query as a cache problem.
	if got := (pgstat.Statement{}).CacheHitRatio(); got != 1 {
		t.Errorf("CacheHitRatio() with no reads = %v, want 1", got)
	}
}

// -- findings ---------------------------------------------------------------

func findRule(findings []Finding, rule string) *Finding {
	for i := range findings {
		if findings[i].Rule == rule {
			return &findings[i]
		}
	}
	return nil
}

func TestDetectsLeadingWildcard(t *testing.T) {
	s := pgstat.Statement{
		Query: "SELECT id FROM articles WHERE title LIKE '%postgres%'",
		Calls: 100, MeanMS: 300, TotalMS: 30000,
	}
	findings := Analyze(s, 0.05, DefaultThresholds())

	f := findRule(findings, "leading-wildcard")
	if f == nil {
		t.Fatal("a leading %% wildcard should be flagged")
	}
	if !strings.Contains(f.Suggest, "trigram") {
		t.Errorf("suggestion should point at pg_trgm, got %q", f.Suggest)
	}
}

func TestDetectsConcatenatedWildcard(t *testing.T) {
	// The ORM-built form, and the only one that survives pg_stat_statements'
	// own parameterisation — so in practice it is the common case, not the
	// exotic one.
	s := pgstat.Statement{
		Query: "SELECT id FROM articles WHERE title LIKE '%' || $1 || '%'",
		Calls: 100, MeanMS: 300, TotalMS: 30000,
	}
	if findRule(Analyze(s, 0.05, DefaultThresholds()), "leading-wildcard") == nil {
		t.Error("a concatenated leading wildcard should be flagged")
	}
}

func TestAnchoredLikeIsNotFlagged(t *testing.T) {
	// 'prefix%' uses a B-tree index perfectly well. Flagging it would make the
	// rule noise.
	s := pgstat.Statement{
		Query: "SELECT id FROM articles WHERE slug LIKE 'postgres%'",
		Calls: 100, MeanMS: 300, TotalMS: 30000,
	}
	if findRule(Analyze(s, 0.05, DefaultThresholds()), "leading-wildcard") != nil {
		t.Error("an anchored LIKE prefix is indexable and must not be flagged")
	}
}

func TestDetectsFunctionWrappedColumn(t *testing.T) {
	s := pgstat.Statement{
		Query: "SELECT id FROM users WHERE lower(email) = 'a@b.com'",
		Calls: 500, MeanMS: 120, TotalMS: 60000,
	}
	f := findRule(Analyze(s, 0.05, DefaultThresholds()), "function-on-column")
	if f == nil {
		t.Fatal("lower(column) in WHERE should be flagged")
	}
	if !strings.Contains(f.Suggest, "CREATE INDEX ON users ((lower(email)))") {
		t.Errorf("expected an expression index on users, got %q", f.Suggest)
	}
}

func TestDetectsNotInSubquery(t *testing.T) {
	s := pgstat.Statement{
		Query: "SELECT id FROM users WHERE id NOT IN (SELECT user_id FROM bans)",
		Calls: 10, MeanMS: 900, TotalMS: 9000,
	}
	f := findRule(Analyze(s, 0.05, DefaultThresholds()), "not-in-subquery")
	if f == nil {
		t.Fatal("NOT IN (SELECT ...) should be flagged")
	}
	if !strings.Contains(f.Why, "NULL") {
		t.Error("the NULL trap is the important half of this finding")
	}
}

func TestDetectsOffsetPagination(t *testing.T) {
	s := pgstat.Statement{
		Query: "SELECT * FROM events ORDER BY created_at DESC LIMIT $1 OFFSET $2",
		Calls: 5000, MeanMS: 80, TotalMS: 400000,
	}
	if findRule(Analyze(s, 0.05, DefaultThresholds()), "offset-pagination") == nil {
		t.Error("OFFSET pagination should be flagged")
	}
}

func TestDetectsTempFileSpill(t *testing.T) {
	s := pgstat.Statement{
		Query: "SELECT * FROM big ORDER BY x", Calls: 5, MeanMS: 2000,
		TotalMS: 10000, TempWrite: 50000,
	}
	f := findRule(Analyze(s, 0.05, DefaultThresholds()), "temp-files")
	if f == nil {
		t.Fatal("temp block writes should be flagged")
	}
	if f.Severity != Critical {
		t.Errorf("spilling to disk should be critical, got %v", f.Severity)
	}
	if !strings.Contains(f.Suggest, "work_mem") {
		t.Error("the actionable setting is work_mem")
	}
}

func TestDominantStatementIsCritical(t *testing.T) {
	s := pgstat.Statement{Query: "SELECT 1", Calls: 1e6, MeanMS: 5, TotalMS: 5e6}
	f := findRule(Analyze(s, 0.42, DefaultThresholds()), "dominant")
	if f == nil {
		t.Fatal("a statement taking 42%% of all time should be flagged")
	}
	if !strings.Contains(f.What, "42%") {
		t.Errorf("the share should be stated, got %q", f.What)
	}
}

func TestUnboundedResultOnlyWithoutLimit(t *testing.T) {
	base := "SELECT * FROM events WHERE user_id = $1"
	unbounded := pgstat.Statement{Query: base, Calls: 10, Rows: 50000, MeanMS: 100}
	bounded := pgstat.Statement{Query: base + " LIMIT $2", Calls: 10, Rows: 50000, MeanMS: 100}

	if findRule(Analyze(unbounded, 0.01, DefaultThresholds()), "unbounded-result") == nil {
		t.Error("a large result with no LIMIT should be flagged")
	}
	if findRule(Analyze(bounded, 0.01, DefaultThresholds()), "unbounded-result") != nil {
		t.Error("a query with LIMIT should not be called unbounded")
	}
}

func TestFastQueryProducesNoFindings(t *testing.T) {
	// The tool is only useful if it stays quiet about healthy queries.
	s := pgstat.Statement{
		Query: "SELECT id, name FROM users WHERE id = $1 LIMIT $2",
		Calls: 100000, MeanMS: 0.4, TotalMS: 40000, Rows: 100000,
		SharedHit: 900000, SharedRead: 100,
	}
	if findings := Analyze(s, 0.02, DefaultThresholds()); len(findings) != 0 {
		t.Errorf("healthy query produced findings: %+v", findings)
	}
}

// -- index suggestions ------------------------------------------------------

func TestSuggestsEqualityColumnsBeforeSortColumn(t *testing.T) {
	// The whole point: a B-tree can only use its ordering once every preceding
	// column is pinned, so the filter must come before the sort.
	suggestion := SuggestIndex(
		"SELECT id FROM events WHERE user_id = $1 AND kind = $2 ORDER BY created_at DESC LIMIT $3")
	if suggestion == nil {
		t.Fatal("expected an index suggestion")
	}

	if suggestion.Table != "events" {
		t.Errorf("table = %q, want events", suggestion.Table)
	}

	last := suggestion.Columns[len(suggestion.Columns)-1]
	if last != "created_at" {
		t.Errorf("the ORDER BY column should come last, got %v", suggestion.Columns)
	}
	if !contains(suggestion.Columns[:len(suggestion.Columns)-1], "user_id") {
		t.Errorf("equality columns should precede the sort, got %v", suggestion.Columns)
	}
	if !strings.Contains(suggestion.DDL, "CONCURRENTLY") {
		t.Error("index creation on a live table should be CONCURRENTLY")
	}
}

func TestNoSuggestionForASingleEqualityColumn(t *testing.T) {
	// Almost always already indexed; suggesting it trains the reader to ignore
	// every suggestion.
	if s := SuggestIndex("SELECT * FROM users WHERE id = $1"); s != nil {
		t.Errorf("expected no suggestion, got %q", s.DDL)
	}
}

func TestNoSuggestionForWrites(t *testing.T) {
	if s := SuggestIndex("UPDATE users SET name = $1 WHERE id = $2 AND org = $3"); s != nil {
		t.Errorf("expected no suggestion for UPDATE, got %q", s.DDL)
	}
}

func TestSuggestionIsCappedAtFourColumns(t *testing.T) {
	suggestion := SuggestIndex(
		"SELECT id FROM t WHERE a = $1 AND b = $2 AND c = $3 AND d = $4 AND e = $5 " +
			"ORDER BY f")
	if suggestion == nil {
		t.Fatal("expected a suggestion")
	}
	if len(suggestion.Columns) > 4 {
		t.Errorf("index has %d columns; past four it costs more than it saves",
			len(suggestion.Columns))
	}
}

func TestWorstSeverity(t *testing.T) {
	findings := []Finding{
		{Severity: Info}, {Severity: Critical}, {Severity: Warning},
	}
	if Worst(findings) != Critical {
		t.Error("Worst should return the highest severity present")
	}
	if Worst(nil) != Info {
		t.Error("no findings should be Info")
	}
}
