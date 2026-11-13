package run

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EquinoxWN/online-schema-migrations/internal/pgtest"
	"github.com/EquinoxWN/online-schema-migrations/internal/plan"
)

const rows = 3000

// setup creates users(id, email NOT NULL) with mixed-case emails and a copy of the originals.
func setup(t *testing.T) (*pgtest.DB, func(raw string) plan.Plan) {
	t.Helper()
	db := pgtest.New(t)
	c := db.Conn()
	db.Exec(c, `CREATE TABLE users (id bigserial PRIMARY KEY, email text NOT NULL)`)
	db.Exec(c, `INSERT INTO users (email) SELECT 'User' || g || '@Example.COM' FROM generate_series(1, $1) g`, rows)
	db.Exec(c, `CREATE TABLE orig AS SELECT id, email FROM users`)
	return db, func(raw string) plan.Plan {
		s, err := plan.ParseSpec([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return plan.Build(s)
	}
}

const emailSpec = `{"id":"email_ci","table":"users","key":"id","kind":"change_type","from":"email","to":"email_ci",
"type":"text","up":"lower(email)","down":"email_ci","not_null":true,"batch_size":250,"lock_timeout":"500ms"}`

// mustRun runs a phase and fails the test on error.
func mustRun(t *testing.T, r *Runner, phase string) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, phase)
	if err != nil {
		t.Fatalf("%s: %v", phase, err)
	}
	return res
}

// TestPGExpandContractEndToEnd runs all four phases while "old" code keeps writing the old
// column, then checks that "new" code works through the trigger and that no data was lost.
func TestPGExpandContractEndToEnd(t *testing.T) {
	db, build := setup(t)
	c := db.Conn()
	r := &Runner{Conn: c, Plan: build(emailSpec)}

	mustRun(t, r, plan.Expand)
	if res := mustRun(t, r, plan.Expand); !res.Skipped {
		t.Fatal("second expand must be a no-op")
	}

	// Old code: inserts and updates using only the old column, during expand..switch.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var oldWrites atomic.Int64
	var writerErr error
	ready := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := db.Conn()
		ctx := context.Background()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var id int64
			if err := w.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, fmt.Sprintf("Old%d@X.ORG", i)).Scan(&id); err != nil {
				writerErr = err
				return
			}
			if _, err := w.Exec(ctx, `UPDATE users SET email = $1 WHERE id = $2`, fmt.Sprintf("Edited%d@X.ORG", i), id); err != nil {
				writerErr = err
				return
			}
			if oldWrites.Add(1) == 20 {
				close(ready) // old code is demonstrably writing before the backfill starts
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(time.Minute):
		close(stop)
		wg.Wait() // writerErr is safe to read only after the writer exits
		t.Fatalf("old-code writer did not start: %v", writerErr)
	}
	before := oldWrites.Load()
	r.MaxBatches = 3
	part := mustRun(t, r, plan.Backfill)
	st, err := r.Status(context.Background())
	if err != nil || st.Phase != StateBackfilling || st.Cursor != 750 || part.Batches != 3 {
		t.Fatalf("after 3 batches: %+v %+v %v", st, part, err)
	}
	r.MaxBatches = 0
	rest := mustRun(t, r, plan.Backfill)
	close(stop)
	wg.Wait()
	if writerErr != nil {
		t.Fatalf("old code failed during the migration: %v", writerErr)
	}
	t.Logf("old code wrote %d rows before and %d during the backfill", before, oldWrites.Load()-before)
	if got := part.Rows + rest.Rows; got != rows {
		t.Errorf("backfill updated %d rows, want %d (rows written by old code are synced by the trigger)", got, rows)
	}
	// The backfill must not touch the old column (the trigger's reverse sync is off during backfill).
	if n := db.Int(c, `SELECT count(*) FROM users u JOIN orig o USING (id) WHERE u.email <> o.email`); n != 0 {
		t.Fatalf("backfill changed %d old values", n)
	}
	mustRun(t, r, plan.Switch)
	if n := db.Int(c, `SELECT count(*) FROM users WHERE email_ci IS DISTINCT FROM lower(email)`); n != 0 {
		t.Fatalf("%d rows out of sync after switch", n)
	}

	// New code writes only the new column; the trigger keeps the old one (NOT NULL) filled.
	db.Exec(c, `INSERT INTO users (email_ci) VALUES ('new@x.org')`)
	db.Exec(c, `UPDATE users SET email_ci = 'changed@x.org' WHERE id = 1`)
	if n := db.Int(c, `SELECT count(*) FROM users WHERE email IN ('new@x.org', 'changed@x.org')`); n != 2 {
		t.Fatalf("old column not maintained for new code: %d", n)
	}

	total := db.Int(c, `SELECT count(*) FROM users`)
	res := mustRun(t, r, plan.Contract)
	if res.Steps != 4 {
		t.Errorf("contract ran %d steps, want 4", res.Steps)
	}
	if n := db.Int(c, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'users' AND column_name = 'email'`); n != 0 {
		t.Error("old column still exists")
	}
	if n := db.Int(c, `SELECT count(*) FROM pg_attribute WHERE attrelid = 'users'::regclass AND attname = 'email_ci' AND attnotnull`); n != 1 {
		t.Error("email_ci is not NOT NULL")
	}
	if n := db.Int(c, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'users'::regclass AND NOT tgisinternal`); n != 0 {
		t.Error("sync trigger still exists")
	}
	if n := db.Int(c, `SELECT count(*) FROM users`); n != total {
		t.Errorf("row count changed from %d to %d", total, n)
	}
	if n := db.Int(c, `SELECT count(*) FROM users u JOIN orig o USING (id) WHERE u.id > 1 AND u.email_ci <> lower(o.email)`); n != 0 {
		t.Errorf("%d migrated values differ from lower(original)", n)
	}
	if res := mustRun(t, r, plan.Contract); !res.Skipped {
		t.Error("second contract must be a no-op")
	}
	st, _ = r.Status(context.Background())
	if st.Phase != StateDone || st.UpdatedAt.Location() != time.UTC {
		t.Errorf("final state %+v", st)
	}
}

// TestPGPhaseOrderIsEnforced checks every out-of-order call and a changed spec are refused.
func TestPGPhaseOrderIsEnforced(t *testing.T) {
	db, build := setup(t)
	r := &Runner{Conn: db.Conn(), Plan: build(emailSpec)}
	ctx := context.Background()
	for _, ph := range []string{plan.Backfill, plan.Switch, plan.Contract, Rollback} {
		if _, err := r.Run(ctx, ph); !errors.Is(err, ErrWrongState) {
			t.Errorf("%s before expand: got %v", ph, err)
		}
	}
	if _, err := r.Run(ctx, "explode"); err == nil {
		t.Error("unknown phase accepted")
	}
	mustRun(t, r, plan.Expand)
	for _, ph := range []string{plan.Switch, plan.Contract} {
		if _, err := r.Run(ctx, ph); !errors.Is(err, ErrWrongState) {
			t.Errorf("%s before backfill: got %v", ph, err)
		}
	}
	changed := &Runner{Conn: db.Conn(), Plan: build(`{"id":"email_ci","table":"users","key":"id","kind":"change_type","from":"email","to":"email_ci","type":"text","up":"upper(email)","down":"email_ci"}`)}
	if _, err := changed.Run(ctx, plan.Backfill); !errors.Is(err, ErrSpecChanged) {
		t.Errorf("changed spec: got %v", err)
	}
}

// TestPGSwitchRefusesOutOfSyncRows checks the switch gate counts rows the trigger missed.
func TestPGSwitchRefusesOutOfSyncRows(t *testing.T) {
	db, build := setup(t)
	c := db.Conn()
	r := &Runner{Conn: c, Plan: build(emailSpec)}
	mustRun(t, r, plan.Expand)
	mustRun(t, r, plan.Backfill)
	db.Exec(c, `ALTER TABLE users DISABLE TRIGGER osm_sync_email_ci`)
	db.Exec(c, `UPDATE users SET email = 'Drift@X' WHERE id IN (5, 6)`)
	if _, err := r.Run(context.Background(), plan.Switch); !errors.Is(err, ErrPending) {
		t.Fatalf("got %v, want ErrPending", err)
	}
	if st, _ := r.Status(context.Background()); st.Phase != StateBackfilled {
		t.Fatalf("state moved to %q", st.Phase)
	}
}

// TestPGLockTimeoutRetriesAreBounded holds a reader open so expand cannot get its lock.
func TestPGLockTimeoutRetriesAreBounded(t *testing.T) {
	db, build := setup(t)
	ctx := context.Background()
	holder := db.Conn()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT count(*) FROM users`); err != nil { // ACCESS SHARE until rollback
		t.Fatal(err)
	}
	p := build(emailSpec)
	p.Spec.LockTimeout = "50ms"
	r := &Runner{Conn: db.Conn(), Plan: p, Retries: 2, Backoff: 10 * time.Millisecond}
	start := time.Now()
	res, err := r.Run(ctx, plan.Expand)
	if !errors.Is(err, ErrLockTimeout) || res.Txns != 3 {
		t.Fatalf("got %v after %d attempts, want ErrLockTimeout after 3", err, res.Txns)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("bounded retries took %s", time.Since(start))
	}
	if st, _ := r.Status(ctx); st.Phase != StateNone {
		t.Fatalf("failed expand recorded state %q", st.Phase)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if res := mustRun(t, r, plan.Expand); res.Txns != 1 {
		t.Fatalf("expand after release took %d attempts", res.Txns)
	}
}

// TestPGRollback undoes expand and a partial backfill, and refuses once contract started.
func TestPGRollback(t *testing.T) {
	db, build := setup(t)
	c := db.Conn()
	r := &Runner{Conn: c, Plan: build(emailSpec), MaxBatches: 2}
	mustRun(t, r, plan.Expand)
	mustRun(t, r, plan.Backfill)
	mustRun(t, r, Rollback)
	if n := db.Int(c, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'users' AND column_name = 'email_ci'`); n != 0 {
		t.Fatal("rollback left the new column")
	}
	if n := db.Int(c, `SELECT count(*) FROM users u JOIN orig o USING (id) WHERE u.email <> o.email`); n != 0 {
		t.Fatal("rollback changed old data")
	}
	if _, err := r.Run(context.Background(), Rollback); !errors.Is(err, ErrWrongState) {
		t.Fatalf("second rollback: %v", err)
	}
	r.MaxBatches = 0
	for _, ph := range plan.Phases {
		mustRun(t, r, ph)
	}
	if _, err := r.Run(context.Background(), Rollback); !errors.Is(err, ErrWrongState) {
		t.Fatalf("rollback after contract: %v", err)
	}
}

// TestPGConcurrentBackfillRunnersDoEachBatchOnce runs two backfills at once.
func TestPGConcurrentBackfillRunnersDoEachBatchOnce(t *testing.T) {
	db, build := setup(t)
	p := build(emailSpec)
	mustRun(t, &Runner{Conn: db.Conn(), Plan: p}, plan.Expand)
	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := range 2 {
		r := &Runner{Conn: db.Conn(), Plan: p}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			results[i], errs[i] = r.Run(ctx, plan.Backfill)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := results[0].Rows + results[1].Rows; got != rows {
		t.Fatalf("two runners updated %d rows in total, want exactly %d", got, rows)
	}
}

// TestPGEmptyTableAndKeyType covers an empty table and a non-integer key.
func TestPGEmptyTableAndKeyType(t *testing.T) {
	db := pgtest.New(t)
	c := db.Conn()
	db.Exec(c, `CREATE TABLE people (id int PRIMARY KEY, name text)`)
	db.Exec(c, `CREATE TABLE tags (code text PRIMARY KEY, label text)`)
	mk := func(raw string) plan.Plan {
		s, err := plan.ParseSpec([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return plan.Build(s)
	}
	r := &Runner{Conn: c, Plan: mk(`{"id":"people_rename","table":"people","key":"id","kind":"rename_column","from":"name","to":"full_name","type":"text"}`)}
	for _, ph := range plan.Phases {
		mustRun(t, r, ph)
	}
	db.Exec(c, `INSERT INTO people (id, full_name) VALUES (1, 'Ada')`)
	bad := &Runner{Conn: c, Plan: mk(`{"id":"tags_rename","table":"tags","key":"code","kind":"rename_column","from":"label","to":"title","type":"text"}`)}
	mustRun(t, bad, plan.Expand)
	if _, err := bad.Run(context.Background(), plan.Backfill); !errors.Is(err, ErrKeyType) {
		t.Fatalf("text key: %v", err)
	}
}
