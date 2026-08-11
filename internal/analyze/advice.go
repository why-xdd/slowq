package analyze

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/why-xdd/slowq/internal/pgstat"
)

// Severity orders findings by how much they usually matter.
type Severity int

const (
	Info Severity = iota
	Warning
	Critical
)

func (s Severity) String() string {
	switch s {
	case Critical:
		return "critical"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// Finding is one observation about a statement, with the reason attached.
//
// The Why field is not decoration. This tool guesses from query text, and a
// suggestion a reader cannot evaluate is a suggestion they will either follow
// blindly or ignore entirely — both worse than understanding it.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"-"`
	Level    string   `json:"severity"`
	What     string   `json:"what"`
	Why      string   `json:"why"`
	Suggest  string   `json:"suggest,omitempty"`
}

// ms formats a duration with enough precision to stay meaningful. "%.0f ms" on
// a 0.8 ms query prints "1 ms", which makes a sub-millisecond statement look
// like a slow one and hides the ratio the reader is trying to judge.
func ms(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f ms", v)
	case v >= 1:
		return fmt.Sprintf("%.1f ms", v)
	default:
		return fmt.Sprintf("%.2f ms", v)
	}
}

func finding(rule string, severity Severity, what, why, suggest string) Finding {
	return Finding{
		Rule: rule, Severity: severity, Level: severity.String(),
		What: what, Why: why, Suggest: suggest,
	}
}

var (
	selectStar = regexp.MustCompile(`(?i)\bSELECT\s+\*`)

	// Checked against the *raw* statement, never the normalised one.
	// Normalisation replaces 'foo%' with ?, which destroys exactly the
	// information this rule needs. That also means the rule is silent on
	// statements pg_stat_statements has already parameterised — which is most
	// of them. It fires on queries captured from logs, on ORM-built patterns
	// that concatenate the wildcard, and on literals that survived. Detecting
	// it sometimes is worth more than never; claiming to detect it always
	// would be worth less than nothing.
	leadingWild  = regexp.MustCompile(`(?i)\bLIKE\s+(?:'%|'\s*\|\||\$?\w*\s*\|\|\s*'%)`)
	notIn        = regexp.MustCompile(`(?i)\bNOT\s+IN\s*\(\s*SELECT\b`)
	orderBy      = regexp.MustCompile(`(?i)\bORDER\s+BY\b`)
	hasLimit     = regexp.MustCompile(`(?i)\bLIMIT\b`)
	offsetLarge  = regexp.MustCompile(`(?i)\bOFFSET\s+\?`)
	distinctAll  = regexp.MustCompile(`(?i)\bSELECT\s+DISTINCT\b`)
	orInWhere    = regexp.MustCompile(`(?i)\bWHERE\b[^;]*?\bOR\b`)
	funcOnColumn = regexp.MustCompile(`(?i)\bWHERE\b[^;]*?\b(lower|upper|date|cast|coalesce|substring)\s*\(\s*([a-z_][a-z0-9_.]*)\s*\)`)
	crossJoin    = regexp.MustCompile(`(?i)\bCROSS\s+JOIN\b`)

	whereEquality = regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_]*(?:\.[a-z_][a-z0-9_]*)?)\s*=\s*\?`)
	orderColumns  = regexp.MustCompile(`(?i)\bORDER\s+BY\s+([a-z_][a-z0-9_.,\s]*?)(?:\s+(?:ASC|DESC))?\s*(?:LIMIT|OFFSET|$)`)
	fromTable     = regexp.MustCompile(`(?i)\bFROM\s+([a-z_][a-z0-9_]*(?:\.[a-z_][a-z0-9_]*)?)`)
)

// Thresholds are the boundaries between "normal" and "worth a look".
//
// Exposed rather than hard-coded because they are genuinely workload-dependent:
// 5 000 rows per call is alarming for an API endpoint and unremarkable for a
// nightly export.
type Thresholds struct {
	SlowMeanMS       float64
	VerySlowMeanMS   float64
	HighRowsPerCall  float64
	LowCacheHitRatio float64
	HighVariability  float64
	DominantShare    float64
}

// DefaultThresholds suit an OLTP service where queries serve user requests.
func DefaultThresholds() Thresholds {
	return Thresholds{
		SlowMeanMS:       50,
		VerySlowMeanMS:   500,
		HighRowsPerCall:  1000,
		LowCacheHitRatio: 0.90,
		HighVariability:  1.0,
		DominantShare:    0.20,
	}
}

// Analyze inspects one statement and returns what is worth saying about it.
//
// Everything here is a heuristic over query *text*. There is no planner and no
// catalog, so this cannot know whether an index already exists or how selective
// a column is. It is a way to decide which twenty queries deserve an EXPLAIN,
// not a replacement for running one.
func Analyze(s pgstat.Statement, share float64, t Thresholds) []Finding {
	query := Normalize(s.Query)
	lower := strings.ToLower(query)
	// Rules that depend on literal values have to read the original text;
	// normalisation is precisely what removes those values.
	raw := strings.ToLower(s.Query)
	var findings []Finding

	if share >= t.DominantShare {
		findings = append(findings, finding("dominant", Critical,
			fmt.Sprintf("%.0f%% of all execution time", share*100),
			"One statement dominating the server is the highest-leverage thing "+
				"on the list: every proportional improvement to it is a proportional "+
				"improvement to the database.",
			"Start here, whatever else the list says."))
	}

	if s.SpillsToDisk() {
		findings = append(findings, finding("temp-files", Critical,
			fmt.Sprintf("wrote %d temp blocks", s.TempWrite),
			"A sort or hash did not fit in work_mem and spilled to disk. Disk is "+
				"orders of magnitude slower than memory, and the spill also evicts "+
				"cache that other queries were using.",
			"Raise work_mem for this workload, or add an index that lets the sort "+
				"be satisfied by an ordered scan instead."))
	}

	switch {
	case s.MeanMS >= t.VerySlowMeanMS:
		findings = append(findings, finding("very-slow", Critical,
			ms(s.MeanMS)+" per call on average",
			"Well past the point where a user notices, and past most client timeouts.",
			"Run EXPLAIN (ANALYZE, BUFFERS) on it."))
	case s.MeanMS >= t.SlowMeanMS:
		findings = append(findings, finding("slow", Warning,
			ms(s.MeanMS)+" per call on average", "", ""))
	}

	if s.Variability() >= t.HighVariability && s.Calls > 10 {
		findings = append(findings, finding("erratic", Warning,
			fmt.Sprintf("%s mean, %s stddev, %s worst",
				ms(s.MeanMS), ms(s.StddevMS), ms(s.MaxMS)),
			"Unpredictable, not just slow. Users experience this as the service "+
				"hanging at random, and averages hide it completely.",
			"Look for lock contention, a plan that flips with parameter values, "+
				"or cache misses on a working set that no longer fits."))
	}

	if ratio := s.CacheHitRatio(); ratio < t.LowCacheHitRatio && s.SharedRead > 1000 {
		findings = append(findings, finding("cache-miss", Warning,
			fmt.Sprintf("%.0f%% buffer cache hit rate", ratio*100),
			"Postgres is going to disk for pages it should be holding in memory.",
			"Either the working set outgrew shared_buffers, or a sequential scan "+
				"is evicting everything else on every call."))
	}

	if s.RowsPerCall() >= t.HighRowsPerCall {
		what := fmt.Sprintf("returns %.0f rows per call", s.RowsPerCall())
		if !hasLimit.MatchString(lower) {
			findings = append(findings, finding("unbounded-result", Warning, what,
				"A large result set with no LIMIT usually means the application "+
					"fetches everything and discards most of it — paying for the "+
					"transfer, the memory, and the parsing.",
				"Add LIMIT, or paginate with a keyset cursor."))
		} else {
			findings = append(findings, finding("large-result", Info, what, "", ""))
		}
	}

	if leadingWild.MatchString(raw) {
		findings = append(findings, finding("leading-wildcard", Warning,
			"LIKE pattern starting with %",
			"A B-tree index is ordered by prefix, so a leading wildcard cannot use "+
				"one. This is a sequential scan no matter what indexes exist.",
			"Use a trigram index (pg_trgm + GIN), or full-text search if the "+
				"column is prose."))
	}

	if m := funcOnColumn.FindStringSubmatch(query); m != nil {
		findings = append(findings, finding("function-on-column", Warning,
			fmt.Sprintf("%s(%s) in WHERE", strings.ToLower(m[1]), m[2]),
			"Wrapping a column in a function discards any plain index on it — the "+
				"index stores the column's values, not the function's results.",
			fmt.Sprintf("CREATE INDEX ON %s ((%s(%s)));",
				tableOf(query), strings.ToLower(m[1]), m[2])))
	}

	if notIn.MatchString(lower) {
		findings = append(findings, finding("not-in-subquery", Warning,
			"NOT IN (SELECT ...)",
			"NOT IN treats NULLs in the subquery as making the whole predicate "+
				"unknown, so a single NULL silently returns zero rows. It also "+
				"plans worse than the alternatives.",
			"Use NOT EXISTS, which is NULL-safe and usually plans as an anti-join."))
	}

	if offsetLarge.MatchString(lower) {
		findings = append(findings, finding("offset-pagination", Warning,
			"OFFSET-based pagination",
			"OFFSET n makes the server produce and discard n rows before returning "+
				"anything, so page 500 costs 500 times page one.",
			"Paginate on the last seen sort key instead: WHERE (created_at, id) < (?, ?)."))
	}

	if selectStar.MatchString(lower) {
		findings = append(findings, finding("select-star", Info,
			"SELECT *",
			"Fetches every column including ones nobody reads, and prevents an "+
				"index-only scan that a narrow column list would allow.",
			"Name the columns you use."))
	}

	if distinctAll.MatchString(lower) {
		findings = append(findings, finding("distinct", Info,
			"SELECT DISTINCT",
			"Often a de-duplication patch over a join that multiplied rows. The "+
				"sort or hash it needs is pure overhead if the join can be fixed.",
			"Check whether an EXISTS subquery removes the need for it."))
	}

	if crossJoin.MatchString(lower) {
		findings = append(findings, finding("cross-join", Warning,
			"CROSS JOIN",
			"Produces every combination of both sides. Occasionally intended, and "+
				"frequently a missing join condition.", ""))
	}

	if orInWhere.MatchString(lower) && s.MeanMS >= t.SlowMeanMS {
		findings = append(findings, finding("or-predicate", Info,
			"OR in WHERE",
			"An OR across different columns often cannot use either column's index, "+
				"because one index cannot satisfy both branches.",
			"A UNION ALL of two indexable queries is sometimes dramatically faster."))
	}

	return findings
}

// IndexSuggestion is a proposed index and the reasoning behind it.
type IndexSuggestion struct {
	Table     string   `json:"table"`
	Columns   []string `json:"columns"`
	DDL       string   `json:"ddl"`
	Rationale string   `json:"rationale"`
}

// SuggestIndex proposes a composite index from the statement's shape.
//
// The column order is the point. Equality predicates come first, then the sort
// columns. A B-tree can only use its ordering after every preceding column is
// pinned to a single value, so an index on (created_at, user_id) does nothing
// for "WHERE user_id = ? ORDER BY created_at" while (user_id, created_at)
// satisfies both the filter and the sort in one scan.
//
// Returns nil when the statement gives no useful shape to work with. Suggesting
// something for every query would train the reader to ignore all of it.
func SuggestIndex(query string) *IndexSuggestion {
	normalized := Normalize(query)
	lower := strings.ToLower(normalized)

	if !strings.HasPrefix(strings.ToUpper(Command(normalized)), "SELECT") {
		return nil
	}

	table := tableOf(normalized)
	if table == "" {
		return nil
	}

	equality := uniqueColumns(whereEquality.FindAllStringSubmatch(normalized, -1), 1)
	var sorts []string
	if m := orderColumns.FindStringSubmatch(normalized); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			if column := bareColumn(part); column != "" {
				sorts = append(sorts, column)
			}
		}
	}

	// One equality column alone is usually already indexed (it is often a
	// foreign key or a primary key), so proposing it adds noise. The
	// combination of a filter and a sort is where indexes are actually missing.
	if len(equality) == 0 || (len(equality) < 2 && len(sorts) == 0) {
		return nil
	}

	columns := append([]string{}, equality...)
	for _, sort := range sorts {
		if !contains(columns, sort) {
			columns = append(columns, sort)
		}
	}
	if len(columns) > 4 {
		// Past four columns the index costs more to maintain than it saves, and
		// the trailing columns are rarely selective enough to matter.
		columns = columns[:4]
	}

	// Order among the equality columns themselves is not significant: a B-tree
	// can seek on any permutation when every one of them is pinned to a single
	// value. They are sorted alphabetically purely so the same query always
	// produces the same suggestion.
	rationale := "equality predicates first, so the B-tree can seek"
	if len(sorts) > 0 && orderBy.MatchString(lower) {
		rationale += "; the ORDER BY column last, so the sort is satisfied by the scan"
	}

	return &IndexSuggestion{
		Table:   table,
		Columns: columns,
		DDL: fmt.Sprintf("CREATE INDEX CONCURRENTLY ON %s (%s);",
			table, strings.Join(columns, ", ")),
		Rationale: rationale,
	}
}

func tableOf(query string) string {
	if m := fromTable.FindStringSubmatch(query); m != nil {
		return m[1]
	}
	return ""
}

func bareColumn(s string) string {
	s = strings.TrimSpace(s)
	s = regexp.MustCompile(`(?i)\s+(ASC|DESC|NULLS\s+(FIRST|LAST))\s*$`).ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "()? ") {
		return ""
	}
	if dot := strings.LastIndex(s, "."); dot >= 0 {
		s = s[dot+1:]
	}
	return strings.ToLower(s)
}

func uniqueColumns(matches [][]string, group int) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		column := bareColumn(m[group])
		if column == "" || seen[column] {
			continue
		}
		seen[column] = true
		out = append(out, column)
	}
	sort.Strings(out)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// Worst returns the highest severity among findings.
func Worst(findings []Finding) Severity {
	worst := Info
	for _, f := range findings {
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}
