# RFC 0001: online-schema-migrations design

- **Status:** Accepted for M1 (M2 and M3 open)
- **Author:** EquinoxWN
- **Created:** 2026-11-11

## Problem

A schema change that looks small can take a busy PostgreSQL table offline. `ALTER TABLE ... ALTER
COLUMN TYPE bigint` rewrites every row while holding `ACCESS EXCLUSIVE`, which blocks reads and
writes for the whole copy; `CREATE INDEX` without `CONCURRENTLY` blocks writes for the whole build;
`SET NOT NULL` scans the table under `ACCESS EXCLUSIVE`. Even a statement that finishes instantly is
dangerous without `lock_timeout`: it queues behind any long-running query, and every query that
arrives after it queues behind it. Renaming or retyping a column in one step also breaks every
running copy of the application that still uses the old column.

The people affected are developers and on-call engineers who write and review migrations for
tables they cannot take offline.

## Goals (M1)

1. **Lock analysis.** For each statement in a migration file, predict the table lock PostgreSQL
   takes, whether it rewrites or scans the table, and whether it blocks reads or writes; flag the
   statement as dangerous when a rewrite or scan happens under a blocking lock on a large table, or
   when it would fail on a non-empty table. Exit non-zero so CI can block the merge.
2. **Expand/contract.** Turn "rename this column" or "change this column's type" into four phases:
   expand (add the new column, dual-write with a trigger), backfill (batched, resumable), switch
   (verify, then read the new column), contract (stop dual-writing, drop the old column), each with
   its rollback, and run them against PostgreSQL with recorded progress.

### Critical-to-quality measures (Define)

| CTQ | Target for M1 | How it is measured |
|---|---|---|
| Lock prediction matches the server | 100% of the transaction-safe statement forms in the test set | `TestPGLockPredictionsMatchServer`: run each statement on PostgreSQL 17 in a transaction, read `pg_locks` and `relfilenode`, compare |
| Dangerous forms are flagged | every rewrite or blocking scan on a table at or above `-max-rows`, every NOT NULL column without a default, every CONCURRENTLY inside a transaction | `TestStatementRules` (58 cases) |
| Plans contain no dangerous statement | 0 danger findings for a 500-million-row table | `TestPlanPassesLockAnalysisOnHugeTable` |
| No data loss during expand/contract with live writes | 0 rows lost or out of sync; old values untouched by the backfill | `TestPGExpandContractEndToEnd` |
| Retries are bounded | exactly `retries + 1` attempts on lock timeout, then an error | `TestPGLockTimeoutRetriesAreBounded` |

## Non-goals

- MySQL (planned after M3; gh-ost's approach differs enough to need its own analyser).
- Shadow-table copies, throttling on replication lag (M2) and cut-over retries under load with k6
  (M3).
- Parsing all of PostgreSQL's grammar. Statements the analyser does not recognise are reported
  as `warn` ("check by hand"), never as safe.
- Running as a hosted service.

## Design

```
migration.sql ──► sqlscan.Split ──► sqlscan.Tokens ──► analyze (rules + per-file state) ──► findings, exit code
                                                          ▲
                         stats.json (row counts, column types) 

spec.json ──► plan.ParseSpec (strict, validated) ──► plan.Build ──► 4 phases of SQL ──► run.Runner ──► PostgreSQL
                                                                         │                    │
                                                                         └─ analyze (test) ◄──┘ osm_state row per change
```

### Lock analysis (`internal/sqlscan`, `internal/analyze`)

- `Split` cuts a file at top-level semicolons and understands single-quoted, `E''`, double-quoted
  and dollar-quoted text and nested block comments, so function bodies stay whole. Input is capped
  at 8 MiB.
- Rules follow the PostgreSQL 17 documentation for `ALTER TABLE`, `CREATE INDEX`, `VACUUM`,
  `CLUSTER`, `REINDEX` and explicit locking (section 13.3). Each `ALTER TABLE` action is reported
  separately. The conflict matrix decides "blocks reads" (conflicts with `ACCESS SHARE`) and
  "blocks writes" (conflicts with `ROW EXCLUSIVE`).
- Per-file state: tables created earlier in the file are empty (no risk); `SET lock_timeout` and
  `SET LOCAL lock_timeout` (cleared at `COMMIT`/`ROLLBACK`); `BEGIN` blocks (for `CONCURRENTLY`);
  `CHECK (col IS NOT NULL)` constraints that were validated, which let `SET NOT NULL` skip its scan
  (PostgreSQL 12+).
- Type changes are rewrites unless the stats file gives the old type and the change is binary
  coercible (wider `varchar`, `varchar` to `text`, wider `numeric` with the same scale).
- Risk: `danger` for a rewrite or scan under a lock that blocks reads or writes on a table with at
  least `-max-rows` rows (default 100,000; unknown tables count as large), a NOT NULL column without
  a default on a non-empty table, `UPDATE`/`DELETE` without `WHERE` on a large table, and
  `CONCURRENTLY` inside a transaction; `warn` for a blocking lock without `lock_timeout`, for
  statements that remove data or names, and for statements the analyser does not know.

### Expand/contract (`internal/plan`, `internal/run`)

- A spec names the table, an integer key, the old and new column, the new type and two SQL
  expressions: `up` (new from old) and `down` (old from new). `rename_column` fills them in.
  Names must be lower-case identifiers and are always quoted; unknown JSON fields are rejected.
- **Expand**, one transaction: `ADD COLUMN` (nullable, no default: no rewrite), a PL/pgSQL
  function and a `BEFORE INSERT OR UPDATE` trigger. The trigger copies old to new when old code
  writes, and new to old when new code writes, so both versions of the application work. During
  the backfill the session sets `osm.backfill = on` and the trigger does not copy back, so the
  backfill never changes old values.
- **Backfill**: the key range `[min, max]` is fixed at the start (later rows are written by the
  trigger); each batch `(lo, hi]` updates only rows that are out of sync and saves the cursor in
  the same transaction, so an interrupted backfill resumes without repeating or skipping a batch.
- **Switch**: counts rows where `new IS DISTINCT FROM up(old)`; any such row stops the switch.
- **Contract**: optional NOT NULL in three steps (`CHECK ... NOT VALID`, `VALIDATE`, `SET NOT NULL`
  using the validated check), then drop the trigger, the function and the old column. Each step is
  its own transaction and the step number is saved with it.
- **Runner**: every transaction sets `lock_timeout` (spec, default 2 s) and `statement_timeout`
  (default 5 min) and takes `pg_advisory_xact_lock` for the change, so two runners never do the
  same batch. A lock timeout (SQLSTATE 55P03) is retried with doubling backoff and jitter, at most
  `retries` times. Progress lives in `osm_state` (`updated_at` is `timestamptz`), with a hash of
  the spec: a run under a changed spec is refused.
- **Rollback** before contract drops the trigger, the function and the new column. After contract
  has started there is no rollback; the plan says so.

## Alternatives considered

| Option | Why not (for M1) |
|---|---|
| Use PostgreSQL's own parser (`pg_query_go`, libpg_query via cgo) | Exact parsing, but needs cgo (not available on the Windows development machine, and it complicates the race-detector build) and still needs every lock rule written by hand: the parser gives the tree, not the locks. The hand-written scanner is small, and its predictions are checked against a real server instead. |
| Ask the server: run each statement in a rolled-back transaction and read `pg_locks` | Exact for the server version, but needs a database with the production schema and, for rewrites, does the full rewrite inside the transaction. Used as the test oracle (`TestPGLockPredictionsMatchServer`), not as the product. |
| Views over versioned schemas (pgroll's approach) | Lets both application versions use the same column name, but every query must go through a schema-qualified view and the search path must be managed per deployment. Expand/contract with a new column name keeps plain tables; see [ADR 0002](../adr/0002-trigger-dual-write.md). |
| Application-side dual writes | No trigger, but every writer (jobs, scripts, other services) must be changed and deployed first, and a missed writer silently leaves rows out of sync. |

## Measurement plan

- M1: the CTQ table above, run locally against PostgreSQL 17.11 (embedded binaries) and in CI
  against the `postgres:17.11` service container with the race detector.
- M3: k6 load during a migration; lock waits (`pg_stat_activity` wait events) and error rate for a
  plain `ALTER TABLE` versus the online method.

## Milestones

- **M1 (done):** lock analysis with risk flags; expand/contract plans and a resumable runner.
- **M2:** for table rewrites, a shadow table kept in sync while rows are copied in small batches;
  backfill throttles itself when replication lag or database load rises.
- **M3:** cut-over takes a brief lock with a strict timeout and retries; k6 measures lock waits and
  errors against a plain `ALTER TABLE`.

## Risks and open questions

- The rules are written for PostgreSQL 14 to 17 (`CREATE OR REPLACE TRIGGER` needs 14). Older or
  newer servers may lock differently; the server test only covers the version it runs on.
- Row counts come from a stats file, not the live catalog; a stale file can call a large table
  small. Unknown tables are treated as large to fail safe.
- The trigger adds work to every write on the table during the change; its cost is not measured
  yet (M3).
- Rewrite detection by `relfilenode` cannot check `CREATE INDEX CONCURRENTLY`, `DROP INDEX
  CONCURRENTLY` and `VACUUM FULL`, which cannot run in a transaction; those rules come from the
  documentation only.
