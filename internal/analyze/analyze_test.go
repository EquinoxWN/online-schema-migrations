package analyze

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testStats = map[string]TableStats{
	"users": {Rows: 500_000_000, Columns: map[string]string{
		"email": "varchar(100)", "price": "numeric(10,2)", "name": "text", "age": "integer",
	}},
	"tiny": {Rows: 10},
	"orgs": {Rows: 2_000_000},
}

// last analyses src and returns the final finding.
func last(t *testing.T, src string) Finding {
	t.Helper()
	fs, err := Analyze(src, Options{Stats: testStats})
	if err != nil {
		t.Fatalf("Analyze(%q): %v", src, err)
	}
	if len(fs) == 0 {
		t.Fatalf("Analyze(%q): no findings", src)
	}
	return fs[len(fs)-1]
}

func TestStatementRules(t *testing.T) {
	const lt = "SET lock_timeout = '2s';\n"
	cases := []struct {
		name    string
		sql     string
		lock    LockMode
		rewrite bool
		scan    bool
		risk    Risk
	}{
		{"add nullable column", lt + "ALTER TABLE users ADD COLUMN bio text", AccessExclusive, false, false, OK},
		{"add column, constant default (PG11+ no rewrite)", lt + "ALTER TABLE users ADD COLUMN flag boolean NOT NULL DEFAULT false", AccessExclusive, false, false, OK},
		{"add column, now() default is stable", lt + "ALTER TABLE users ADD COLUMN at timestamptz DEFAULT now()", AccessExclusive, false, false, OK},
		{"add column, volatile default rewrites", lt + "ALTER TABLE users ADD COLUMN token uuid DEFAULT gen_random_uuid()", AccessExclusive, true, false, Danger},
		{"add serial column rewrites", lt + "ALTER TABLE users ADD COLUMN n bigserial", AccessExclusive, true, false, Danger},
		{"add identity column rewrites", lt + "ALTER TABLE users ADD COLUMN n int GENERATED ALWAYS AS IDENTITY", AccessExclusive, true, false, Danger},
		{"add NOT NULL without default fails", lt + "ALTER TABLE users ADD COLUMN x int NOT NULL", AccessExclusive, false, false, Danger},
		{"add NOT NULL DEFAULT NULL still fails", lt + "ALTER TABLE users ADD COLUMN x int DEFAULT NULL NOT NULL", AccessExclusive, false, false, Danger},
		{"add column with inline unique builds index", lt + "ALTER TABLE users ADD COLUMN x int UNIQUE", AccessExclusive, false, true, Danger},
		{"add column without lock_timeout", "ALTER TABLE users ADD COLUMN bio text", AccessExclusive, false, false, Warn},
		{"drop column", lt + "ALTER TABLE users DROP COLUMN bio", AccessExclusive, false, false, Warn},
		{"type change, unknown old type", lt + "ALTER TABLE users ALTER COLUMN zzz TYPE bigint", AccessExclusive, true, false, Danger},
		{"type change int to bigint rewrites", lt + "ALTER TABLE users ALTER COLUMN age TYPE bigint", AccessExclusive, true, false, Danger},
		{"varchar widened is binary coercible", lt + "ALTER TABLE users ALTER COLUMN email TYPE varchar(200)", AccessExclusive, false, false, OK},
		{"varchar narrowed rewrites", lt + "ALTER TABLE users ALTER COLUMN email TYPE varchar(50)", AccessExclusive, true, false, Danger},
		{"varchar to text", lt + "ALTER TABLE users ALTER COLUMN email SET DATA TYPE text", AccessExclusive, false, false, OK},
		{"USING forces rewrite", lt + "ALTER TABLE users ALTER COLUMN email TYPE text USING lower(email)", AccessExclusive, true, false, Danger},
		{"numeric precision widened", lt + "ALTER TABLE users ALTER COLUMN price TYPE numeric(12,2)", AccessExclusive, false, false, OK},
		{"numeric scale changed rewrites", lt + "ALTER TABLE users ALTER COLUMN price TYPE numeric(12,3)", AccessExclusive, true, false, Danger},
		{"set not null scans", lt + "ALTER TABLE users ALTER COLUMN name SET NOT NULL", AccessExclusive, false, true, Danger},
		{"set not null on small table", lt + "ALTER TABLE tiny ALTER COLUMN name SET NOT NULL", AccessExclusive, false, true, OK},
		{"set default", lt + "ALTER TABLE users ALTER COLUMN name SET DEFAULT 'x'", AccessExclusive, false, false, OK},
		{"set statistics", "ALTER TABLE users ALTER COLUMN name SET STATISTICS 500", ShareUpdateExclusive, false, false, OK},
		{"check constraint scans", lt + "ALTER TABLE users ADD CONSTRAINT c CHECK (age > 0)", AccessExclusive, false, true, Danger},
		{"check constraint not valid", lt + "ALTER TABLE users ADD CONSTRAINT c CHECK (age > 0) NOT VALID", AccessExclusive, false, false, OK},
		{"validate constraint", "ALTER TABLE users VALIDATE CONSTRAINT c", ShareUpdateExclusive, false, true, OK},
		{"foreign key scans", lt + "ALTER TABLE users ADD CONSTRAINT fk FOREIGN KEY (org_id) REFERENCES orgs (id)", ShareRowExclusive, false, true, Danger},
		{"foreign key not valid", lt + "ALTER TABLE users ADD CONSTRAINT fk FOREIGN KEY (org_id) REFERENCES orgs (id) NOT VALID", ShareRowExclusive, false, false, OK},
		{"foreign key, big referenced table", lt + "ALTER TABLE tiny ADD FOREIGN KEY (org_id) REFERENCES orgs (id)", ShareRowExclusive, false, true, Danger},
		{"unique constraint builds index", lt + "ALTER TABLE users ADD CONSTRAINT u UNIQUE (email)", AccessExclusive, false, true, Danger},
		{"unique using index", lt + "ALTER TABLE users ADD CONSTRAINT u UNIQUE USING INDEX users_email_idx", AccessExclusive, false, false, OK},
		{"create index blocks writes", "CREATE INDEX i ON users (email)", Share, false, true, Danger},
		{"create index concurrently", "CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON users (email)", ShareUpdateExclusive, false, true, OK},
		{"create unique index without name", "CREATE UNIQUE INDEX ON tiny (x)", Share, false, true, Warn},
		{"concurrently in a transaction", "BEGIN; CREATE INDEX CONCURRENTLY i ON users (email); COMMIT", ShareUpdateExclusive, false, true, Danger},
		{"drop index", "DROP INDEX i", AccessExclusive, false, false, Warn},
		{"drop index concurrently", "DROP INDEX CONCURRENTLY IF EXISTS i", ShareUpdateExclusive, false, false, OK},
		{"rename column breaks readers", lt + "ALTER TABLE users RENAME COLUMN name TO full_name", AccessExclusive, false, false, Warn},
		{"set tablespace rewrites", lt + "ALTER TABLE users SET TABLESPACE fast", AccessExclusive, true, false, Danger},
		{"storage parameters", "ALTER TABLE users SET (fillfactor = 70)", ShareUpdateExclusive, false, false, OK},
		{"vacuum full rewrites", "VACUUM (FULL, ANALYZE) users", AccessExclusive, true, false, Danger},
		{"plain vacuum", "VACUUM ANALYZE users", ShareUpdateExclusive, false, false, OK},
		{"cluster rewrites", "CLUSTER users", AccessExclusive, true, false, Danger},
		{"reindex table", "REINDEX TABLE users", Share, false, true, Danger},
		{"reindex concurrently", "REINDEX TABLE CONCURRENTLY users", ShareUpdateExclusive, false, true, OK},
		{"update without where", "UPDATE users SET name = lower(name)", RowExclusive, false, true, Danger},
		{"update with where", "UPDATE users SET name = 'x' WHERE id = 1", RowExclusive, false, false, OK},
		{"update without where, small table", "UPDATE tiny SET name = 'x'", RowExclusive, false, true, OK},
		{"delete without where", "DELETE FROM users", RowExclusive, false, true, Danger},
		{"truncate", lt + "TRUNCATE users", AccessExclusive, false, false, Warn},
		{"lock table share mode", lt + "LOCK TABLE users IN SHARE MODE", Share, false, false, OK},
		{"create trigger", "CREATE OR REPLACE TRIGGER t BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION f()", ShareRowExclusive, false, false, Warn},
		{"create function takes no table lock", "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$", NoLock, false, false, OK},
		{"unknown statement", "REFRESH MATERIALIZED VIEW mv", NoLock, false, false, Warn},
		{"lock_timeout 0 disables it", "SET lock_timeout = 0; ALTER TABLE users ADD COLUMN bio text", AccessExclusive, false, false, Warn},
		{"SET LOCAL ends at commit", "BEGIN; SET LOCAL lock_timeout = '1s'; COMMIT; ALTER TABLE users ADD COLUMN bio text", AccessExclusive, false, false, Warn},
		{"SET LOCAL inside the transaction", "BEGIN; SET LOCAL lock_timeout TO '1s'; ALTER TABLE users ADD COLUMN bio text", AccessExclusive, false, false, OK},
		{"unknown table treated as large", lt + "ALTER TABLE ghosts ALTER COLUMN a SET NOT NULL", AccessExclusive, false, true, Danger},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := last(t, c.sql)
			if f.Lock != c.lock || f.Rewrite != c.rewrite || f.Scan != c.scan || f.Risk != c.risk {
				t.Errorf("got lock=%s rewrite=%t scan=%t risk=%s %v; want lock=%s rewrite=%t scan=%t risk=%s",
					f.Lock, f.Rewrite, f.Scan, f.Risk, f.Reasons, c.lock, c.rewrite, c.scan, c.risk)
			}
		})
	}
}

func TestEachAlterActionIsReported(t *testing.T) {
	fs, err := Analyze("SET lock_timeout = '1s'; ALTER TABLE users ADD COLUMN a int, ALTER COLUMN age TYPE bigint, ADD CONSTRAINT c CHECK (a IN (1, 2)) NOT VALID", Options{Stats: testStats})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 3 {
		t.Fatalf("got %d findings, want 3 (one per action): %+v", len(fs), fs)
	}
	if fs[0].Risk != OK || fs[1].Risk != Danger || fs[2].Risk != OK || Worst(fs) != Danger {
		t.Errorf("risks %s %s %s, worst %s", fs[0].Risk, fs[1].Risk, fs[2].Risk, Worst(fs))
	}
}

func TestNewTablesAreExempt(t *testing.T) {
	fs, err := Analyze("CREATE TABLE IF NOT EXISTS fresh (id bigint); CREATE INDEX ON fresh (id); ALTER TABLE fresh ALTER COLUMN id TYPE text", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Risk != OK || f.BlocksReads || f.BlocksWrites || f.Rewrite || f.Scan {
			t.Errorf("%s on a table created in the same file should be harmless: %+v", f.Op, f)
		}
	}
}

func TestValidatedNotNullCheckAvoidsScan(t *testing.T) {
	src := `SET lock_timeout = '2s';
ALTER TABLE users ADD CONSTRAINT nn CHECK (name IS NOT NULL) NOT VALID;
ALTER TABLE users VALIDATE CONSTRAINT nn;
ALTER TABLE users ALTER COLUMN name SET NOT NULL;`
	f := last(t, src)
	if f.Scan || f.Risk != OK {
		t.Fatalf("SET NOT NULL after a validated check should not scan: %+v", f)
	}
	// Without VALIDATE the check proves nothing.
	f = last(t, "SET lock_timeout = '2s'; ALTER TABLE users ADD CONSTRAINT nn CHECK (name IS NOT NULL) NOT VALID; ALTER TABLE users ALTER COLUMN name SET NOT NULL")
	if !f.Scan || f.Risk != Danger {
		t.Fatalf("unvalidated check must not skip the scan: %+v", f)
	}
}

func TestMaxRowsThreshold(t *testing.T) {
	src := "SET lock_timeout = '1s'; ALTER TABLE orgs ALTER COLUMN a SET NOT NULL"
	for _, c := range []struct {
		max  int64
		want Risk
	}{{2_000_000, Danger}, {2_000_001, OK}} {
		fs, err := Analyze(src, Options{Stats: testStats, MaxRows: c.max})
		if err != nil {
			t.Fatal(err)
		}
		if got := fs[len(fs)-1].Risk; got != c.want {
			t.Errorf("MaxRows=%d: risk %s, want %s", c.max, got, c.want)
		}
	}
}

func TestSchemaQualifiedNamesUseUnqualifiedStats(t *testing.T) {
	f := last(t, "SET lock_timeout = '1s'; ALTER TABLE public.tiny ALTER COLUMN a SET NOT NULL")
	if f.Table != "public.tiny" || f.Risk != OK {
		t.Fatalf("public.tiny should use the stats of tiny (10 rows): %+v", f)
	}
}

func TestAnalyzeReportsLinesAndBadInput(t *testing.T) {
	fs, err := Analyze("\n\nCREATE INDEX i ON users (a);", Options{Stats: testStats})
	if err != nil || len(fs) != 1 || fs[0].Line != 3 || fs[0].Statement != "CREATE INDEX i ON users (a)" {
		t.Fatalf("got %+v, %v", fs, err)
	}
	if _, err := Analyze("ALTER TABLE 'oops", Options{}); err == nil {
		t.Fatal("unterminated string must be an error")
	}
	if fs, err := Analyze("", Options{}); err != nil || len(fs) != 0 || Worst(fs) != OK {
		t.Fatalf("empty input: %v %v", fs, err)
	}
}

func TestBinaryCoercible(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{"varchar(10)", "varchar(10)", true},
		{"varchar(10)", "varchar(11)", true},
		{"varchar(10)", "varchar(9)", false},
		{"character varying(10)", "text", true},
		{"varchar(10)", "varchar", true},
		{"varchar", "varchar(10)", false},
		{"text", "varchar", true},
		{"text", "varchar(10)", false},
		{"numeric(10,2)", "numeric(12,2)", true},
		{"numeric(10,2)", "numeric(9,2)", false},
		{"numeric(10,2)", "numeric(12,3)", false},
		{"numeric(10)", "numeric(12,0)", true},
		{"numeric(10,2)", "numeric", true},
		{"integer", "bigint", false},
		{"timestamp", "timestamptz", false},
		{"BIGINT", "bigint", true},
	}
	for _, c := range cases {
		if got := BinaryCoercible(c.from, c.to); got != c.want {
			t.Errorf("BinaryCoercible(%q, %q) = %t, want %t", c.from, c.to, got, c.want)
		}
	}
}

func TestConflictMatrix(t *testing.T) {
	for a := AccessShare; a <= AccessExclusive; a++ {
		for b := AccessShare; b <= AccessExclusive; b++ {
			if Conflicts(a, b) != Conflicts(b, a) {
				t.Errorf("matrix not symmetric for %s / %s", a, b)
			}
		}
		if !Conflicts(a, AccessExclusive) {
			t.Errorf("%s must conflict with ACCESS EXCLUSIVE", a)
		}
	}
	// Self-conflicting modes per the PostgreSQL docs.
	self := map[LockMode]bool{ShareUpdateExclusive: true, ShareRowExclusive: true, Exclusive: true, AccessExclusive: true}
	for m := AccessShare; m <= AccessExclusive; m++ {
		if Conflicts(m, m) != self[m] {
			t.Errorf("Conflicts(%s, %s) = %t", m, m, Conflicts(m, m))
		}
	}
	readers := 0
	for m := AccessShare; m <= AccessExclusive; m++ {
		if m.BlocksReads() {
			readers++
		}
	}
	if readers != 1 || !AccessExclusive.BlocksReads() {
		t.Errorf("only ACCESS EXCLUSIVE blocks plain reads, got %d modes", readers)
	}
	if !Share.BlocksWrites() || ShareUpdateExclusive.BlocksWrites() || RowExclusive.BlocksWrites() {
		t.Error("SHARE blocks writes; SHARE UPDATE EXCLUSIVE and ROW EXCLUSIVE do not")
	}
	if Conflicts(NoLock, AccessExclusive) || Conflicts(LockMode(99), AccessShare) {
		t.Error("out-of-range modes never conflict")
	}
	if m, ok := ModeFromPG("ShareUpdateExclusiveLock"); !ok || m != ShareUpdateExclusive {
		t.Error("ModeFromPG")
	}
	if _, ok := ModeFromPG("bogus"); ok {
		t.Error("ModeFromPG accepted bogus")
	}
	if LockMode(42).String() != "LockMode(42)" || Risk(7).String() != "Risk(7)" {
		t.Error("String of out-of-range values")
	}
}

func TestLoadStats(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	st, err := LoadStats(write("ok.json", `{"tables":{"users":{"rows":5,"columns":{"a":"int"}}}}`))
	if err != nil || st["users"].Rows != 5 || st["users"].Columns["a"] != "int" {
		t.Fatalf("got %+v, %v", st, err)
	}
	for name, body := range map[string]string{
		"neg.json":     `{"tables":{"users":{"rows":-1}}}`,
		"unknown.json": `{"tables":{},"extra":1}`,
		"missing.json": `{}`,
		"empty.json":   `{"tables":{" ":{"rows":1}}}`,
		"bad.json":     `not json`,
	} {
		if _, err := LoadStats(write(name, body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
	if _, err := LoadStats(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestFindingJSON(t *testing.T) {
	f := last(t, "CREATE INDEX i ON users (a)")
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"lock":"SHARE"`, `"risk":"danger"`, `"blocks_writes":true`, `"blocks_reads":false`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON %s lacks %s", b, want)
		}
	}
}
