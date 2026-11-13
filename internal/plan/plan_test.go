package plan

import (
	"strings"
	"testing"

	"github.com/EquinoxWN/online-schema-migrations/internal/analyze"
)

const changeTypeJSON = `{"id":"email_ci","table":"users","key":"id","kind":"change_type",
"from":"email","to":"email_ci","type":"text","up":"lower(email)","down":"email_ci","not_null":true}`

func mustSpec(t *testing.T, raw string) Spec {
	t.Helper()
	s, err := ParseSpec([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	return s
}

func TestParseSpecDefaultsAndRename(t *testing.T) {
	s := mustSpec(t, `{"id":"r1","table":"users","key":"id","kind":"rename_column","from":"name","to":"full_name","type":"TEXT"}`)
	if s.Up != `"name"` || s.Down != `"full_name"` || s.BatchSize != DefaultBatch || s.LockTimeout != "2s" || s.Type != "text" {
		t.Fatalf("defaults not applied: %+v", s)
	}
	if s.LockTimeoutDuration().Milliseconds() != 2000 {
		t.Fatal("lock timeout duration")
	}
}

func TestParseSpecRejectsBadInput(t *testing.T) {
	base := map[string]string{
		"id": `"x"`, "table": `"users"`, "key": `"id"`, "kind": `"change_type"`, "from": `"a"`, "to": `"b"`,
		"type": `"text"`, "up": `"a::text"`, "down": `"b"`,
	}
	build := func(over map[string]string) string {
		var parts []string
		for k, v := range base {
			if o, ok := over[k]; ok {
				v = o
			}
			if v != "" {
				parts = append(parts, `"`+k+`":`+v)
			}
		}
		for k, v := range over {
			if _, ok := base[k]; !ok {
				parts = append(parts, `"`+k+`":`+v)
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	if _, err := ParseSpec([]byte(build(nil))); err != nil {
		t.Fatalf("base spec must be valid: %v", err)
	}
	bad := map[string]map[string]string{
		"empty id":            {"id": `""`},
		"id with dash":        {"id": `"a-b"`},
		"quoted table":        {"table": `"Users"`},
		"injection in table":  {"table": `"users; drop table x"`},
		"same from and to":    {"to": `"a"`},
		"key is from":         {"key": `"a"`},
		"type with semicolon": {"type": `"text; drop"`},
		"unknown kind":        {"kind": `"split"`},
		"missing up":          {"up": ""},
		"semicolon in up":     {"up": `"a; DROP TABLE users"`},
		"unbalanced parens":   {"up": `"lower(a"`},
		"closing first":       {"up": `")a("`},
		"dollar tag in down":  {"down": `"$osm$"`},
		"unterminated string": {"down": `"'abc"`},
		"long expression":     {"up": `"` + strings.Repeat("a", 1001) + `"`},
		"batch too large":     {"batch_size": `100001`},
		"batch negative":      {"batch_size": `-1`},
		"lock timeout tiny":   {"lock_timeout": `"1ms"`},
		"lock timeout junk":   {"lock_timeout": `"soon"`},
		"unknown field":       {"extra": `1`},
	}
	for name, over := range bad {
		if _, err := ParseSpec([]byte(build(over))); err == nil {
			t.Errorf("%s: accepted %s", name, build(over))
		}
	}
	rename := `{"id":"r","table":"t","key":"id","kind":"rename_column","from":"a","to":"b","type":"int","up":"a"}`
	if _, err := ParseSpec([]byte(rename)); err == nil {
		t.Error("rename_column with an up expression accepted")
	}
	if _, err := ParseSpec([]byte(build(nil) + `{}`)); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := ParseSpec([]byte(`[]`)); err == nil {
		t.Error("non-object accepted")
	}
}

func TestBuildHasFourPhasesInOrder(t *testing.T) {
	p := Build(mustSpec(t, changeTypeJSON))
	if len(p.Phases) != 4 {
		t.Fatalf("got %d phases", len(p.Phases))
	}
	for i, name := range Phases {
		if p.Phases[i].Name != name || p.Phases[i].Rollback == "" || len(p.Phases[i].Steps) == 0 {
			t.Errorf("phase %d = %+v, want %s with steps and a rollback", i, p.Phases[i], name)
		}
	}
	exp := strings.Join(ExpandSQL(p.Spec), "\n")
	for _, want := range []string{`ADD COLUMN IF NOT EXISTS "email_ci" text`, "CREATE OR REPLACE TRIGGER", "osm.backfill",
		`NEW."email_ci" := (SELECT (lower(email))`, `NEW."email" := (SELECT (email_ci)`} {
		if !strings.Contains(exp, want) {
			t.Errorf("expand SQL lacks %q:\n%s", want, exp)
		}
	}
	if strings.Contains(exp, "NOT NULL DEFAULT") || strings.Contains(strings.ToUpper(ExpandSQL(p.Spec)[0]), "DEFAULT") {
		t.Error("expand must add the column without a default")
	}
	if got := BackfillSQL(p.Spec); got != `UPDATE "users" SET "email_ci" = (lower(email)) WHERE "id" > $1 AND "id" <= $2 AND "email_ci" IS DISTINCT FROM (lower(email))` {
		t.Errorf("backfill SQL: %s", got)
	}
	steps := ContractSteps(p.Spec)
	if len(steps) != 4 || !strings.Contains(steps[0].SQL[0], "NOT VALID") || !strings.Contains(steps[3].SQL[2], `DROP COLUMN IF EXISTS "email"`) {
		t.Errorf("contract steps: %+v", steps)
	}
	noNN := p.Spec
	noNN.NotNull = false
	if len(ContractSteps(noNN)) != 1 {
		t.Error("without not_null the contract is one step")
	}
	if rb := strings.Join(RollbackSQL(p.Spec), ";"); !strings.Contains(rb, `DROP COLUMN IF EXISTS "email_ci"`) || strings.Contains(rb, `"email";`) {
		t.Errorf("rollback must drop only the new column: %s", rb)
	}
}

// The whole point: every statement the planner emits is safe on a 500-million-row table.
func TestPlanPassesLockAnalysisOnHugeTable(t *testing.T) {
	for _, raw := range []string{changeTypeJSON,
		`{"id":"r","table":"users","key":"id","kind":"rename_column","from":"name","to":"full_name","type":"text"}`} {
		p := Build(mustSpec(t, raw))
		fs, err := analyze.Analyze(p.Script(), analyze.Options{Stats: map[string]analyze.TableStats{"users": {Rows: 500_000_000}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(fs) < 5 {
			t.Fatalf("only %d findings from the plan script", len(fs))
		}
		for _, f := range fs {
			if f.Risk == analyze.Danger {
				t.Errorf("plan %s emits a dangerous statement: %+v", p.Spec.ID, f)
			}
			if f.Rewrite {
				t.Errorf("plan %s rewrites the table: %s", p.Spec.ID, f.Statement)
			}
			if f.Scan && (f.BlocksReads || f.BlocksWrites) && f.Op != "UPDATE" {
				t.Errorf("plan %s scans under a blocking lock: %s", p.Spec.ID, f.Statement)
			}
		}
	}
}

func TestHashChangesWithSpec(t *testing.T) {
	a := mustSpec(t, changeTypeJSON)
	b := a
	b.BatchSize = 5
	if a.Hash() == b.Hash() || a.Hash() != mustSpec(t, changeTypeJSON).Hash() || len(a.Hash()) != 16 {
		t.Fatal("hash must be stable for equal specs and differ for changed ones")
	}
}
