# ADR 0003: Hand-written SQL scanner, checked against a real server

- **Status:** Accepted

## Context

The lock analyser has to read migration files. PostgreSQL's own parser is available to Go only
through cgo (`pg_query_go`), and even an exact syntax tree does not say which lock a statement
takes: the lock rules have to be written by hand either way. The development machine has no C
compiler, and CI builds with the race detector.

## Decision

Use a small scanner (`internal/sqlscan`: statement splitting, quoting, dollar quotes, comments)
and keyword rules for the DDL forms that matter for locking. Every rule for a statement that can
run inside a transaction is checked by `TestPGLockPredictionsMatchServer`, which runs it on a real
PostgreSQL 17 and compares `pg_locks` and `relfilenode` with the prediction. Anything the rules do
not recognise is reported as `warn`, never as safe.

## Consequences

- No cgo and no third-party parser; the module depends only on `pgx` (for the runner).
- The rules cover a subset of the grammar. Unusual syntax can be misread; the server test and the
  "not recognised" warning limit the damage, and new rules come with a server test row.
- Locally the server is PostgreSQL 17.11 from the zonky embedded binaries in `.tmp/`; in CI it is
  the `postgres:17.11` image. Other server versions are not tested.
