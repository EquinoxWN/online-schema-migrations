# ADR 0002: Dual-write with a database trigger and a new column name

- **Status:** Accepted

## Context

During expand/contract, old code (writing the old column) and new code (writing the new column)
run at the same time. Both columns must stay in sync until the old one is dropped. Options:

1. Application dual writes: each writer updates both columns.
2. A `BEFORE INSERT OR UPDATE` trigger that copies one column to the other.
3. pgroll-style versioned views, so both versions see the same column name.

## Decision

Use a trigger (option 2), and give the new column a new name. The trigger copies old to new
(`up`) when the old column changes and new to old (`down`) when only the new column changes; the
backfill marks its session with `osm.backfill = on` so its own updates are not copied back.

## Consequences

- Every writer is covered, including jobs and manual SQL, without changing them first.
- Every write to the table pays for the trigger until contract; the cost is not measured yet (M3).
- The application must switch to the new column name; a type change cannot keep the old name
  without views. This is the trade-off against option 3, which keeps names but routes every query
  through a versioned schema.
- `down` must be a real inverse for rows new code writes; a lossy `up` (such as `lower(email)`)
  means the old column receives the new value as written. The spec documents both directions.
- `CREATE OR REPLACE TRIGGER` needs PostgreSQL 14 or newer.
