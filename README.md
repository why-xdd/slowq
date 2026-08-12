<div align="center">

# slowq

**Find the Postgres queries that actually cost you time — and understand why.**
A single static binary over `pg_stat_statements`. No agent, no dashboard, no server.

[![CI](https://github.com/why-xdd/slowq/actions/workflows/ci.yml/badge.svg)](https://github.com/why-xdd/slowq/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-12%2B-4169E1?logo=postgresql&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-green)

<img width="580" src="https://raw.githubusercontent.com/why-xdd/slowq/main/docs/banner.svg" alt="slowq — rank Postgres queries by total time, not duration"/>

</div>

---

```bash
go install github.com/why-xdd/slowq/cmd/slowq@latest

slowq -dsn "$DATABASE_URL"
```

```
slowq
8 statements · 1.7h total execution time · 9.3M calls · showing top 2

● #1 7c6c8baefd40b749
  1.1h total (63.9%) · 4.8M calls · 0.81ms mean · 812ms max · 2 rows/call
  SELECT id, user_id, kind, payload, created_at FROM events WHERE user_id = ? AND kind =…
    ✗ 64% of all execution time
      One statement dominating the server is the highest-leverage thing on the list:
      every proportional improvement to it is a proportional improvement to the
      database.
      Start here, whatever else the list says.
    ! 0.81 ms mean, 4.9 ms stddev, 812 ms worst
      Unpredictable, not just slow. Users experience this as the service hanging at
      random, and averages hide it completely.
      Look for lock contention, a plan that flips with parameter values, or cache misses
      on a working set that no longer fits.
    ▸ index candidate
      CREATE INDEX CONCURRENTLY ON events (kind, user_id, created_at);
      equality predicates first, so the B-tree can seek; the ORDER BY column last, so
      the sort is satisfied by the scan

● #2 386360edcd58a361
  18.4m total (18.0%) · 128 calls · 8.6s mean · 21.4s max · 69844 rows/call
  SELECT * FROM orders o JOIN order_items i ON i.order_id = o.id JOIN customers c ON…
    ✗ wrote 244000 temp blocks
      A sort or hash did not fit in work_mem and spilled to disk. Disk is orders of
      magnitude slower than memory, and the spill also evicts cache that other queries
      were using.
      Raise work_mem for this workload, or add an index that lets the sort be satisfied
      by an ordered scan instead.
```

Try it without a database:

```bash
slowq -file testdata/snapshot.json
```

<img src="https://raw.githubusercontent.com/why-xdd/slowq/main/docs/terminal.png" alt="slowq output: the 0.81ms query taking 64% of all execution time, ranked above an 8.6s report" width="100%"/>

---

## The one idea

**Rank by total time, not by duration.** The query worth fixing is almost never
the slowest one.

Look at the output above. The nightly report at 8.6 seconds per call *looks*
like the emergency. It is not — it runs 128 times and accounts for 18% of the
server. The 0.81 ms query above it accounts for **64%**, because it runs 4.8
million times. Halving it recovers more than deleting the report entirely.

Sorting `pg_stat_statements` by `mean_exec_time` — which is what most people
type first — puts these in exactly the wrong order.

---

## What it tells you, and why

Every finding carries its reasoning. The tool guesses from query text, so a
suggestion you cannot evaluate is one you will either follow blindly or ignore
completely — both worse than understanding it.

| finding | what it means |
|---|---|
| **dominant** | One statement is most of your database. Nothing else on the list matters as much. |
| **temp-files** | A sort or hash outgrew `work_mem` and spilled to disk, evicting everyone else's cache on the way. |
| **erratic** | High variance, not just high mean. This is the one users describe as "it sometimes just hangs" — and averages hide it completely. |
| **cache-miss** | Postgres is reading from disk pages it should be holding in memory. |
| **unbounded-result** | Thousands of rows per call and no `LIMIT`. The app is almost certainly discarding most of them. |
| **leading-wildcard** | `LIKE '%…'` cannot use a B-tree. Points at `pg_trgm`. |
| **function-on-column** | `WHERE lower(email) = ?` discards any plain index on `email`, and emits the expression index that fixes it. |
| **not-in-subquery** | `NOT IN (SELECT …)` returns zero rows if the subquery yields a single NULL. |
| **offset-pagination** | `OFFSET n` produces and throws away n rows, so page 500 costs 500× page one. |

### Index suggestions put the columns in the right order

```sql
CREATE INDEX CONCURRENTLY ON events (kind, user_id, created_at);
```

Column order is the whole game. A B-tree can only use its ordering once every
preceding column is pinned to a single value — so an index on
`(created_at, user_id)` does **nothing** for
`WHERE user_id = ? ORDER BY created_at`, while `(user_id, created_at)` satisfies
the filter *and* the sort in one scan.

Order among the equality columns themselves does not matter; they are sorted
alphabetically so the same query always yields the same suggestion.

It stays quiet when there is nothing useful to say — a single equality column is
almost always indexed already, and a tool that suggests something for every query
trains you to ignore all of it.

---

## Snapshots and diffs

Cumulative counters answer *"what has been slow since the last stats reset"*.
During an incident the question is *"what changed in the last ten minutes"*, and
only a difference of two snapshots answers that.

```bash
slowq -dsn "$DATABASE_URL" -save before.json
# ... wait, or deploy, or let the incident develop ...
slowq -dsn "$DATABASE_URL" -save after.json
slowq -file before.json -diff after.json
```

The delta reports the window's own mean rather than the lifetime one, drops
statements that did not run, and keeps statements that appeared for the first
time — which is frequently the regression you are looking for.

Snapshots also decouple capture from analysis: take one on a production host no
laptop can reach, analyse it anywhere, attach it to the incident review
afterwards.

---

## Usage

```
Sources (one of):
  -dsn string     Postgres connection string, or $DATABASE_URL
  -file string    JSON snapshot written by -save, or "-" for stdin

  -diff string    second snapshot; report the delta between the two
  -save string    write the snapshot to this path
  -sort string    total | mean | calls | rows | variance   (default "total")
  -limit int      statements to show                        (default 20)
  -slow-ms float  mean duration considered slow             (default 50)
  -verbose        print full queries, re-indented
  -json           machine-readable output
```

```bash
slowq -sort variance          # what hangs unpredictably
slowq -sort rows              # what returns far more than it should
slowq -json | jq '.[] | select(.findings[].rule == "temp-files")'
```

`-json` emits every finding and index suggestion, so this drops into a cron job
or a CI check as easily as it runs by hand.

---

## Setup

`pg_stat_statements` ships with Postgres but is not enabled by default:

```sql
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
```

```ini
# postgresql.conf — needs a restart
shared_preload_libraries = 'pg_stat_statements'
pg_stat_statements.max = 10000
pg_stat_statements.track = all
```

slowq handles the Postgres 13 column rename (`total_time` → `total_exec_time`)
by checking `server_version_num` first, so 12 and 16 both work.

---

## Tests

```bash
go test ./...
```

No database required — the analysis is pure functions over statement text and
counters, and the fixtures are JSON. The suite pins the things that are easy to
get quietly wrong:

- **`IN ($1,$2,$3)` and `IN ($1..$8)` must fingerprint identically.** Otherwise
  one problem appears as a hundred rows and none of them looks big enough to fix.
- **A healthy query must produce zero findings.** A linter that always says
  something gets ignored, which costs more than it ever saved.
- **Diffing must survive `pg_stat_statements_reset()`**, which makes the later
  snapshot *smaller*. Reporting negative durations would be worse than nothing.
- **Anchored `LIKE 'prefix%'` must not be flagged** — it uses an index perfectly
  well, and flagging it would make the rule noise.

One test caught a rule that could never have fired: the leading-wildcard check
originally ran against the *normalised* query, where `'%foo%'` has already
become `?`. It now reads the raw text — which also means it is honestly silent
on statements `pg_stat_statements` has already parameterised, and says so in the
code rather than pretending to a coverage it does not have.

---

## What this is not

Not a monitoring product. There is no agent, no time series, no web UI — and
crucially, **no planner**. slowq reads query text and counters, so it cannot know
whether an index already exists or how selective a column is.

It is a way to decide which twenty queries out of ten thousand deserve an
`EXPLAIN (ANALYZE, BUFFERS)`. The footer says so every run.

For continuous monitoring use pganalyze or pgwatch2. For the ten minutes when
something is on fire and you need to know where to look, this is one binary and
no setup.

MIT © [why-xdd](https://github.com/why-xdd)
