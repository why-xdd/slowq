package pgstat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

// The column names changed in Postgres 13: total_time became total_exec_time
// when planning time was split out. Querying the wrong set fails outright, so
// the version is checked first rather than guessed.
const queryPG13 = `
SELECT queryid, query, calls,
       total_exec_time, min_exec_time, max_exec_time,
       mean_exec_time, stddev_exec_time, rows,
       shared_blks_hit, shared_blks_read,
       temp_blks_read, temp_blks_written
FROM pg_stat_statements
WHERE query NOT LIKE '%%pg_stat_statements%%'
ORDER BY total_exec_time DESC
LIMIT $1`

const queryPG12 = `
SELECT queryid, query, calls,
       total_time, min_time, max_time,
       mean_time, stddev_time, rows,
       shared_blks_hit, shared_blks_read,
       temp_blks_read, temp_blks_written
FROM pg_stat_statements
WHERE query NOT LIKE '%%pg_stat_statements%%'
ORDER BY total_time DESC
LIMIT $1`

// FromPostgres reads a live snapshot.
func FromPostgres(ctx context.Context, dsn string, limit int) (Snapshot, error) {
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return Snapshot{}, fmt.Errorf("connect: %w", err)
	}
	defer connection.Close(ctx)

	// current_setting rather than SHOW: SHOW returns its value as text, and
	// scanning that into an int fails against every real server - a bug that
	// unit tests cannot see, because they never open a connection.
	var version int
	if err := connection.QueryRow(
		ctx, "SELECT current_setting('server_version_num')::int",
	).Scan(&version); err != nil {
		return Snapshot{}, fmt.Errorf("read server version: %w", err)
	}

	statement := queryPG13
	if version < 130000 {
		statement = queryPG12
	}

	rows, err := connection.Query(ctx, statement, limit)
	if err != nil {
		return Snapshot{}, fmt.Errorf(
			"query pg_stat_statements (is the extension installed? "+
				"CREATE EXTENSION pg_stat_statements): %w", err)
	}
	defer rows.Close()

	snapshot := Snapshot{TakenAt: time.Now().UTC(), Version: fmt.Sprint(version)}
	for rows.Next() {
		var s Statement
		if err := rows.Scan(
			&s.QueryID, &s.Query, &s.Calls,
			&s.TotalMS, &s.MinMS, &s.MaxMS,
			&s.MeanMS, &s.StddevMS, &s.Rows,
			&s.SharedHit, &s.SharedRead,
			&s.TempRead, &s.TempWrite,
		); err != nil {
			return Snapshot{}, fmt.Errorf("scan row: %w", err)
		}
		snapshot.Statements = append(snapshot.Statements, s)
	}

	return snapshot, rows.Err()
}

// FromJSON reads a snapshot previously written by Save, or "-" for stdin.
//
// This is what makes the tool usable without a database in reach: capture on a
// production host that no laptop can connect to, analyse it anywhere, and
// attach it to the incident review afterwards.
func FromJSON(path string) (Snapshot, error) {
	var data []byte
	var err error

	if path == "-" {
		data, err = os.ReadFile(os.Stdin.Name())
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read %s: %w", path, err)
	}

	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		// Also accept a bare array, which is what `psql -c '... \gx' --json`
		// style exports and most hand-rolled dumps produce.
		var statements []Statement
		if err2 := json.Unmarshal(data, &statements); err2 != nil {
			return Snapshot{}, fmt.Errorf("parse %s: %w", path, err)
		}
		snapshot = Snapshot{Statements: statements, TakenAt: time.Now().UTC()}
	}

	return snapshot, nil
}

// Save writes a snapshot as JSON.
func Save(snapshot Snapshot, path string) error {
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Diff returns per-statement deltas between two snapshots, matched on queryid.
//
// Cumulative counters answer "what has been slow since the last stats reset",
// which is rarely the question during an incident. The question is "what
// changed in the last ten minutes", and only a difference of two snapshots
// answers that.
func Diff(before, after Snapshot) Snapshot {
	previous := make(map[int64]Statement, len(before.Statements))
	for _, s := range before.Statements {
		previous[s.QueryID] = s
	}

	delta := Snapshot{TakenAt: after.TakenAt, Version: after.Version}
	for _, current := range after.Statements {
		old, seen := previous[current.QueryID]
		if !seen {
			delta.Statements = append(delta.Statements, current)
			continue
		}

		calls := current.Calls - old.Calls
		if calls <= 0 {
			continue // not executed in the window, so it is not the answer
		}

		d := current
		d.Calls = calls
		d.TotalMS = current.TotalMS - old.TotalMS
		d.Rows = current.Rows - old.Rows
		d.SharedHit = current.SharedHit - old.SharedHit
		d.SharedRead = current.SharedRead - old.SharedRead
		d.TempRead = current.TempRead - old.TempRead
		d.TempWrite = current.TempWrite - old.TempWrite
		d.MeanMS = d.TotalMS / float64(calls)
		// min, max and stddev are not differenceable — they describe the whole
		// history, not the window. Carrying them over would be a lie, so the
		// window's mean is the only central measure reported.
		d.StddevMS = 0

		delta.Statements = append(delta.Statements, d)
	}

	return delta
}
