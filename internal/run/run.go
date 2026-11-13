// Package run executes an expand/contract plan against PostgreSQL, one recorded phase at a time.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EquinoxWN/online-schema-migrations/internal/plan"
)

// Conn is the subset of *pgx.Conn the runner uses.
type Conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Recorded states of a change, in order.
const (
	StateNone        = ""
	StateExpanded    = "expanded"
	StateBackfilling = "backfilling"
	StateBackfilled  = "backfilled"
	StateSwitched    = "switched"
	StateContracting = "contracting"
	StateDone        = "done"
)

// Rollback is the pseudo-phase that undoes expand.
const Rollback = "rollback"

// Errors callers can test for.
var (
	ErrSpecChanged = errors.New("spec changed since this change started (hash differs); finish or roll back the old one first")
	ErrWrongState  = errors.New("phase not allowed in the current state")
	ErrPending     = errors.New("rows still out of sync")
	ErrLockTimeout = errors.New("lock not acquired within lock_timeout after all retries")
	ErrKeyType     = errors.New("key column must be smallint, integer or bigint")
)

// lockNotAvailable is SQLSTATE 55P03, raised when lock_timeout expires.
const lockNotAvailable = "55P03"

// State is the recorded progress of one change.
type State struct {
	Phase     string    `json:"phase"`
	Step      int       `json:"step"`
	Cursor    int64     `json:"cursor"`
	Target    int64     `json:"target"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Result summarises one Run call.
type Result struct {
	Phase    string        `json:"phase"`
	Skipped  bool          `json:"skipped"`
	Steps    int           `json:"steps"`
	Batches  int           `json:"batches"`
	Rows     int64         `json:"rows"`
	Txns     int           `json:"transactions"` // transactions started, retries included
	Duration time.Duration `json:"duration_ns"`
}

// Runner executes phases of one plan.
type Runner struct {
	Conn             Conn
	Plan             plan.Plan
	Retries          int           // extra attempts after a lock timeout (default 5)
	Backoff          time.Duration // first retry delay, doubled each time, capped at 10s (default 200ms)
	StatementTimeout time.Duration // per statement (default 5m)
	MaxBatches       int           // stop the backfill after this many batches; 0 means no limit
	Log              io.Writer     // progress lines; nil discards
	attempts         int
}

// StateTableSQL creates the progress table.
const StateTableSQL = `CREATE TABLE IF NOT EXISTS osm_state (
  id text PRIMARY KEY,
  spec_hash text NOT NULL,
  phase text NOT NULL,
  step integer NOT NULL DEFAULT 0,
  cursor bigint NOT NULL DEFAULT 0,
  target bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now()
)`

// Run executes one phase; repeating a finished phase is a no-op.
func (r *Runner) Run(ctx context.Context, phase string) (Result, error) {
	start := time.Now()
	r.attempts = 0
	if _, err := r.Conn.Exec(ctx, StateTableSQL); err != nil {
		return Result{}, fmt.Errorf("create osm_state: %w", err)
	}
	res := Result{Phase: phase}
	var err error
	switch phase {
	case plan.Expand:
		err = r.expand(ctx, &res)
	case plan.Backfill:
		err = r.backfill(ctx, &res)
	case plan.Switch:
		err = r.switchReads(ctx, &res)
	case plan.Contract:
		err = r.contract(ctx, &res)
	case Rollback:
		err = r.rollback(ctx, &res)
	default:
		return Result{}, fmt.Errorf("unknown phase %q (want expand, backfill, switch, contract or rollback)", phase)
	}
	res.Txns, res.Duration = r.attempts, time.Since(start)
	return res, err
}

// Status returns the recorded state, Phase "" if the change never started.
func (r *Runner) Status(ctx context.Context) (State, error) {
	if _, err := r.Conn.Exec(ctx, StateTableSQL); err != nil {
		return State{}, fmt.Errorf("create osm_state: %w", err)
	}
	return r.load(ctx, r.Conn)
}

// querier is satisfied by both Conn and pgx.Tx.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// load reads the state row and checks the spec hash.
func (r *Runner) load(ctx context.Context, q querier) (State, error) {
	var st State
	var hash string
	err := q.QueryRow(ctx, `SELECT spec_hash, phase, step, cursor, target, updated_at FROM osm_state WHERE id = $1`, r.Plan.Spec.ID).
		Scan(&hash, &st.Phase, &st.Step, &st.Cursor, &st.Target, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read osm_state: %w", err)
	}
	if hash != r.Plan.Spec.Hash() {
		return State{}, ErrSpecChanged
	}
	st.UpdatedAt = st.UpdatedAt.UTC()
	return st, nil
}

// save upserts the state row inside tx.
func (r *Runner) save(ctx context.Context, tx pgx.Tx, st State) error {
	_, err := tx.Exec(ctx, `INSERT INTO osm_state (id, spec_hash, phase, step, cursor, target, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (id) DO UPDATE SET phase = EXCLUDED.phase, step = EXCLUDED.step, cursor = EXCLUDED.cursor,
  target = EXCLUDED.target, updated_at = now()`,
		r.Plan.Spec.ID, r.Plan.Spec.Hash(), st.Phase, st.Step, st.Cursor, st.Target)
	if err != nil {
		return fmt.Errorf("write osm_state: %w", err)
	}
	return nil
}

// txFunc is work done inside one transaction with the change's advisory lock held.
type txFunc func(ctx context.Context, tx pgx.Tx, st State) error

// inTx runs fn in a transaction with timeouts set, retrying on lock timeouts.
func (r *Runner) inTx(ctx context.Context, fn txFunc) error {
	retries, backoff := r.Retries, r.Backoff
	if retries <= 0 {
		retries = 5
	}
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	for attempt := 0; ; attempt++ {
		r.attempts++
		err := r.once(ctx, fn)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != lockNotAvailable {
			return err
		}
		if attempt >= retries {
			return fmt.Errorf("%w (%d attempts): %v", ErrLockTimeout, attempt+1, err)
		}
		d := min(backoff<<attempt, 10*time.Second)
		d += time.Duration(rand.Int64N(int64(d)/2 + 1)) // jitter so retrying runners do not align
		r.logf("lock timeout, retry %d/%d in %s", attempt+1, retries, d.Round(time.Millisecond))
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// once runs fn in a single transaction.
func (r *Runner) once(ctx context.Context, fn txFunc) error {
	tx, err := r.Conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // no-op after commit
	stmt := r.StatementTimeout
	if stmt <= 0 {
		stmt = 5 * time.Minute
	}
	for _, q := range []string{
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", r.Plan.Spec.LockTimeoutDuration().Milliseconds()),
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", stmt.Milliseconds()),
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("set timeouts: %w", err)
		}
	}
	// Serialises runners of the same change; released at commit or rollback.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('osm:' || $1))`, r.Plan.Spec.ID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	st, err := r.load(ctx, tx)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx, st); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// errSkip ends a transaction early without error when the phase is already done.
var errSkip = errors.New("skip")

// expand adds the column, function and trigger in one transaction.
func (r *Runner) expand(ctx context.Context, res *Result) error {
	err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx, st State) error {
		if st.Phase != StateNone {
			return errSkip
		}
		for _, q := range plan.ExpandSQL(r.Plan.Spec) {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("expand: %w", err)
			}
		}
		res.Steps = 1
		return r.save(ctx, tx, State{Phase: StateExpanded})
	})
	return r.skipped(err, res, "expand")
}

// skipped turns errSkip into a successful no-op.
func (r *Runner) skipped(err error, res *Result, phase string) error {
	if errors.Is(err, errSkip) {
		res.Skipped = true
		r.logf("%s already done, nothing to do", phase)
		return nil
	}
	return err
}

// backfill copies rows in key-range batches, one committed transaction per batch.
func (r *Runner) backfill(ctx context.Context, res *Result) error {
	s := r.Plan.Spec
	var typ string
	err := r.Conn.QueryRow(ctx, `SELECT data_type FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, s.Table, s.Key).Scan(&typ)
	if err != nil {
		return fmt.Errorf("look up key column %s.%s: %w", s.Table, s.Key, err)
	}
	if typ != "smallint" && typ != "integer" && typ != "bigint" {
		return fmt.Errorf("%w (%s is %s)", ErrKeyType, s.Key, typ)
	}
	err = r.inTx(ctx, func(ctx context.Context, tx pgx.Tx, st State) error {
		switch st.Phase {
		case StateBackfilling:
			return nil
		case StateExpanded:
		case StateBackfilled, StateSwitched, StateContracting, StateDone:
			return errSkip
		default:
			return fmt.Errorf("%w: backfill needs expand first (state %q)", ErrWrongState, st.Phase)
		}
		var lo, hi *int64
		q := fmt.Sprintf(`SELECT min(%[1]s), max(%[1]s) FROM %[2]s`, `"`+s.Key+`"`, `"`+s.Table+`"`)
		if err := tx.QueryRow(ctx, q).Scan(&lo, &hi); err != nil {
			return fmt.Errorf("key range: %w", err)
		}
		next := State{Phase: StateBackfilling}
		if lo == nil {
			next.Phase = StateBackfilled // empty table: the trigger covers every future row
		} else {
			next.Cursor, next.Target = *lo-1, *hi // rows inserted later are written by the trigger
		}
		return r.save(ctx, tx, next)
	})
	if err = r.skipped(err, res, "backfill"); err != nil || res.Skipped {
		return err
	}
	update := plan.BackfillSQL(s)
	for {
		if r.MaxBatches > 0 && res.Batches >= r.MaxBatches {
			r.logf("stopped after %d batches; run backfill again to resume", res.Batches)
			return nil
		}
		done := false
		err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx, st State) error {
			if st.Phase != StateBackfilling {
				done = true
				return errSkip
			}
			if st.Cursor >= st.Target {
				done = true
				st.Phase = StateBackfilled
				return r.save(ctx, tx, st)
			}
			if _, err := tx.Exec(ctx, `SET LOCAL osm.backfill = 'on'`); err != nil {
				return fmt.Errorf("mark backfill session: %w", err)
			}
			hi := min(st.Cursor+int64(s.BatchSize), st.Target)
			tag, err := tx.Exec(ctx, update, st.Cursor, hi)
			if err != nil {
				return fmt.Errorf("backfill (%d, %d]: %w", st.Cursor, hi, err)
			}
			res.Rows += tag.RowsAffected()
			st.Cursor = hi
			return r.save(ctx, tx, st)
		})
		if err != nil && !errors.Is(err, errSkip) {
			return err
		}
		if done {
			r.logf("backfill done: %d batches, %d rows updated", res.Batches, res.Rows)
			return nil
		}
		res.Batches++
	}
}

// switchReads checks every row is in sync before code may read the new column.
func (r *Runner) switchReads(ctx context.Context, res *Result) error {
	err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx, st State) error {
		switch st.Phase {
		case StateBackfilled:
		case StateSwitched, StateContracting, StateDone:
			return errSkip
		default:
			return fmt.Errorf("%w: switch needs a finished backfill (state %q)", ErrWrongState, st.Phase)
		}
		var pending int64
		if err := tx.QueryRow(ctx, plan.PendingSQL(r.Plan.Spec)).Scan(&pending); err != nil {
			return fmt.Errorf("count out-of-sync rows: %w", err)
		}
		if pending > 0 {
			return fmt.Errorf("%w: %d rows; run backfill again", ErrPending, pending)
		}
		res.Steps = 1
		st.Phase = StateSwitched
		return r.save(ctx, tx, st)
	})
	return r.skipped(err, res, "switch")
}

// contract runs each contract step in its own transaction, resuming after the last committed one.
func (r *Runner) contract(ctx context.Context, res *Result) error {
	steps := plan.ContractSteps(r.Plan.Spec)
	for {
		finished := false
		err := r.inTx(ctx, func(ctx context.Context, tx pgx.Tx, st State) error {
			switch st.Phase {
			case StateSwitched, StateContracting:
			case StateDone:
				return errSkip
			default:
				return fmt.Errorf("%w: contract needs switch first (state %q)", ErrWrongState, st.Phase)
			}
			if st.Step >= len(steps) {
				finished = true
				st.Phase = StateDone
				return r.save(ctx, tx, st)
			}
			for _, q := range steps[st.Step].SQL {
				if _, err := tx.Exec(ctx, q); err != nil {
					return fmt.Errorf("contract step %q: %w", steps[st.Step].Name, err)
				}
			}
			r.logf("contract step %d/%d done: %s", st.Step+1, len(steps), steps[st.Step].Name)
			st.Phase, st.Step = StateContracting, st.Step+1
			res.Steps++
			return r.save(ctx, tx, st)
		})
		if err != nil {
			if res.Steps == 0 {
				return r.skipped(err, res, "contract")
			}
			return err
		}
		if finished {
			return nil
		}
	}
}

// rollback undoes expand while the old column still exists.
func (r *Runner) rollback(ctx context.Context, res *Result) error {
	return r.inTx(ctx, func(ctx context.Context, tx pgx.Tx, st State) error {
		switch st.Phase {
		case StateExpanded, StateBackfilling, StateBackfilled, StateSwitched:
		case StateNone:
			return fmt.Errorf("%w: nothing to roll back", ErrWrongState)
		default:
			return fmt.Errorf("%w: contract has started (state %q); the old column may be gone", ErrWrongState, st.Phase)
		}
		for _, q := range plan.RollbackSQL(r.Plan.Spec) {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("rollback: %w", err)
			}
		}
		res.Steps = 1
		if _, err := tx.Exec(ctx, `DELETE FROM osm_state WHERE id = $1`, r.Plan.Spec.ID); err != nil {
			return fmt.Errorf("clear osm_state: %w", err)
		}
		return nil
	})
}

// logf writes one progress line.
func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		_, _ = fmt.Fprintf(r.Log, format+"\n", args...) // progress output only; a failed write must not stop a migration
	}
}
