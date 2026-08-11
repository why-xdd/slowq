// Package analyze turns raw statement text into something a person can read
// and something a machine can group.
package analyze

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	// pg_stat_statements already replaces literals with $1, $2 when
	// pg_stat_statements.track_utility and normalisation are on. It does not
	// when statements arrive from a log or an older extension version, so
	// normalising again is idempotent rather than redundant.
	stringLiteral = regexp.MustCompile(`'(?:[^']|'')*'`)
	numberLiteral = regexp.MustCompile(`\b\d+\.?\d*\b`)
	placeholder   = regexp.MustCompile(`\$\d+`)

	// An IN list with 3 items and one with 3000 are the same query shape. Left
	// alone they fingerprint differently and the ranking splits one problem
	// across a hundred rows.
	inList = regexp.MustCompile(`(?is)\bIN\s*\(\s*(?:\?|\$\d+)(?:\s*,\s*(?:\?|\$\d+))*\s*\)`)

	whitespace  = regexp.MustCompile(`\s+`)
	lineComment = regexp.MustCompile(`--[^\n]*`)
	blockNoise  = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// Normalize reduces a statement to its shape: literals become placeholders,
// comments and whitespace collapse, and variable-length IN lists become one
// canonical form.
//
// Two statements with the same normal form should be the same problem. That is
// the whole basis for aggregating them.
func Normalize(query string) string {
	q := blockNoise.ReplaceAllString(query, " ")
	q = lineComment.ReplaceAllString(q, " ")

	q = stringLiteral.ReplaceAllString(q, "?")
	q = placeholder.ReplaceAllString(q, "?")
	q = numberLiteral.ReplaceAllString(q, "?")

	q = inList.ReplaceAllString(q, "IN (?)")
	q = whitespace.ReplaceAllString(q, " ")

	return strings.TrimSpace(q)
}

// Fingerprint is a stable identifier for a query shape.
//
// Postgres' own queryid is not portable: it changes across major versions and
// differs between servers, so it cannot be used to compare a snapshot taken
// today against one taken before an upgrade. This can.
func Fingerprint(query string) string {
	sum := sha1.Sum([]byte(strings.ToLower(Normalize(query))))
	return hex.EncodeToString(sum[:8])
}

var keywords = regexp.MustCompile(`(?i)\b(SELECT|INSERT|UPDATE|DELETE|FROM|WHERE|` +
	`JOIN|LEFT|RIGHT|INNER|OUTER|GROUP BY|ORDER BY|HAVING|LIMIT|OFFSET|UNION|` +
	`WITH|VALUES|SET|RETURNING|ON CONFLICT)\b`)

// Pretty re-indents a statement so a long query is readable in a terminal,
// putting each major clause on its own line.
func Pretty(query string) string {
	q := whitespace.ReplaceAllString(strings.TrimSpace(query), " ")
	q = keywords.ReplaceAllStringFunc(q, func(kw string) string {
		return "\n" + strings.ToUpper(kw)
	})
	return strings.TrimSpace(q)
}

// Truncate shortens a string for table display, on a word boundary when it can.
func Truncate(s string, max int) string {
	s = whitespace.ReplaceAllString(strings.TrimSpace(s), " ")
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}

	cut := s[:max-1]
	// Only break on a space if one is reasonably close to the end; otherwise a
	// query with no spaces near the limit would lose most of its length.
	if space := strings.LastIndex(cut, " "); space > max*3/4 {
		cut = cut[:space]
	}
	return cut + "…"
}

// Command returns the leading SQL verb: SELECT, UPDATE, and so on.
func Command(query string) string {
	fields := strings.Fields(strings.TrimSpace(query))
	for _, field := range fields {
		upper := strings.ToUpper(strings.Trim(field, "(;"))
		switch upper {
		case "WITH", "EXPLAIN", "ANALYZE":
			continue // a CTE's verb is whatever comes after it
		case "":
			continue
		default:
			return upper
		}
	}
	return "UNKNOWN"
}
