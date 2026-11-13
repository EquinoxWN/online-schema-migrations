package analyze

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EquinoxWN/online-schema-migrations/internal/pgtest"
)

// TestPGLockPredictionsMatchServer runs each statement on a real server inside a transaction and
// compares the strongest lock it holds on the table, and whether the table file was replaced
// (a rewrite), with the analyser's prediction.
func TestPGLockPredictionsMatchServer(t *testing.T) {
	db := pgtest.New(t)
	c := db.Conn()
	for _, q := range []string{
		`CREATE TABLE users (id bigint PRIMARY KEY, email varchar(100), price numeric(10,2), name text, age integer, org_id bigint)`,
		`CREATE TABLE orgs (id bigint PRIMARY KEY)`,
		`CREATE UNIQUE INDEX users_email_uidx ON users (email)`,
		`CREATE INDEX users_age_idx ON users (age)`,
		`ALTER TABLE users ADD CONSTRAINT c_age CHECK (age >= 0) NOT VALID`,
		`CREATE FUNCTION noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`,
		`INSERT INTO orgs SELECT g FROM generate_series(1, 10) g`,
		`INSERT INTO users SELECT g, 'u' || g, g, 'n' || g, g % 90, 1 + g % 10 FROM generate_series(1, 200) g`,
	} {
		db.Exec(c, q)
	}
	stats := map[string]TableStats{"users": {Rows: 200, Columns: map[string]string{
		"email": "varchar(100)", "price": "numeric(10,2)", "name": "text", "age": "integer"}}, "orgs": {Rows: 10}}

	cases := []struct {
		sql         string
		skipRewrite bool // TRUNCATE swaps the file without copying rows
	}{
		{sql: "ALTER TABLE users ADD COLUMN bio text"},
		{sql: "ALTER TABLE users ADD COLUMN flag boolean NOT NULL DEFAULT false"},
		{sql: "ALTER TABLE users ADD COLUMN at timestamptz DEFAULT now()"},
		{sql: "ALTER TABLE users ADD COLUMN token float8 DEFAULT random()"},
		{sql: "ALTER TABLE users ADD COLUMN n bigserial"},
		{sql: "ALTER TABLE users ADD COLUMN n int GENERATED ALWAYS AS IDENTITY"},
		{sql: "ALTER TABLE users DROP COLUMN name"},
		{sql: "ALTER TABLE users ALTER COLUMN age TYPE bigint"},
		{sql: "ALTER TABLE users ALTER COLUMN email TYPE varchar(200)"},
		{sql: "ALTER TABLE users ALTER COLUMN email TYPE varchar(50)"},
		{sql: "ALTER TABLE users ALTER COLUMN email TYPE text"},
		{sql: "ALTER TABLE users ALTER COLUMN price TYPE numeric(12,2)"},
		{sql: "ALTER TABLE users ALTER COLUMN price TYPE numeric(12,3)"},
		{sql: "ALTER TABLE users ALTER COLUMN name TYPE text USING upper(name)"},
		{sql: "ALTER TABLE users ALTER COLUMN name SET NOT NULL"},
		{sql: "ALTER TABLE users ALTER COLUMN name SET DEFAULT 'x'"},
		{sql: "ALTER TABLE users ALTER COLUMN name SET STATISTICS 500"},
		{sql: "ALTER TABLE users ADD CONSTRAINT c CHECK (age < 1000)"},
		{sql: "ALTER TABLE users ADD CONSTRAINT c CHECK (age < 1000) NOT VALID"},
		{sql: "ALTER TABLE users VALIDATE CONSTRAINT c_age"},
		{sql: "ALTER TABLE users ADD CONSTRAINT fk FOREIGN KEY (org_id) REFERENCES orgs (id)"},
		{sql: "ALTER TABLE users ADD CONSTRAINT fk FOREIGN KEY (org_id) REFERENCES orgs (id) NOT VALID"},
		{sql: "ALTER TABLE users ADD CONSTRAINT u UNIQUE (name)"},
		{sql: "ALTER TABLE users ADD CONSTRAINT u UNIQUE USING INDEX users_email_uidx"},
		{sql: "ALTER TABLE users RENAME COLUMN name TO full_name"},
		{sql: "ALTER TABLE users SET (fillfactor = 70)"},
		{sql: "ALTER TABLE users SET UNLOGGED"},
		{sql: "CREATE INDEX i ON users (name)"},
		{sql: "CREATE TRIGGER t BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION noop()"},
		{sql: "CLUSTER users USING users_pkey"},
		{sql: "REINDEX TABLE users"},
		{sql: "UPDATE users SET name = lower(name)"},
		{sql: "DELETE FROM users WHERE id = 1"},
		{sql: "LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE"},
		{sql: "TRUNCATE users CASCADE", skipRewrite: true},
	}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			fs, err := Analyze(tc.sql, Options{Stats: stats})
			if err != nil || len(fs) != 1 {
				t.Fatalf("analyse: %v %v", fs, err)
			}
			want := fs[0]
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			tx, err := c.Begin(cctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }() // every case is undone
			var before, after int64
			fileQ := `SELECT relfilenode::int8 FROM pg_class WHERE oid = 'users'::regclass`
			if err := tx.QueryRow(cctx, fileQ).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(cctx, tc.sql); err != nil {
				t.Fatalf("server rejected it: %v", err)
			}
			if err := tx.QueryRow(cctx, fileQ).Scan(&after); err != nil {
				t.Fatal(err)
			}
			got := heldMode(t, cctx, tx, "users")
			if got != want.Lock {
				t.Errorf("server holds %s, analyser predicted %s", got, want.Lock)
			}
			if !tc.skipRewrite && (before != after) != want.Rewrite {
				t.Errorf("server rewrote=%t, analyser predicted rewrite=%t", before != after, want.Rewrite)
			}
			if want.Referenced != "" {
				if ref := heldMode(t, cctx, tx, want.Referenced); ref != ShareRowExclusive {
					t.Errorf("referenced table %s: server holds %s, want SHARE ROW EXCLUSIVE", want.Referenced, ref)
				}
			}
		})
	}
}

// TestPGNotNullWithoutDefaultFails confirms the analyser's "fails on a table that has rows" rule.
func TestPGNotNullWithoutDefaultFails(t *testing.T) {
	db := pgtest.New(t)
	c := db.Conn()
	db.Exec(c, "CREATE TABLE t (id int)")
	db.Exec(c, "INSERT INTO t VALUES (1)")
	const q = "ALTER TABLE t ADD COLUMN x int NOT NULL"
	fs, err := Analyze(q, Options{Stats: map[string]TableStats{"t": {Rows: 1}}})
	if err != nil || fs[0].Risk != Danger {
		t.Fatalf("analyser: %+v %v", fs, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "23502") {
		t.Fatalf("server should reject it with a not-null violation (23502), got %v", err)
	}
}

// heldMode returns the strongest lock this backend holds on a table.
func heldMode(t *testing.T, ctx context.Context, tx pgx.Tx, table string) LockMode {
	t.Helper()
	rows, err := tx.Query(ctx, `SELECT mode FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'relation'
AND relation = to_regclass($1)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	best := NoLock
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		if lm, ok := ModeFromPG(m); ok && lm > best {
			best = lm
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return best
}
