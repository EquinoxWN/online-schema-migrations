package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EquinoxWN/online-schema-migrations/internal/pgtest"
)

// call runs the CLI with a fixed environment.
func call(env map[string]string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := cli(context.Background(), args, &out, &errOut, func(k string) string { return env[k] })
	return code, out.String(), errOut.String()
}

const ex = "../../examples/"

func TestUsageAndUnknownCommands(t *testing.T) {
	if code, _, e := call(nil); code != exitError || !strings.Contains(e, "usage") {
		t.Errorf("no args: %d %q", code, e)
	}
	if code, o, _ := call(nil, "help"); code != exitOK || !strings.Contains(o, "osm analyze") {
		t.Errorf("help: %d", code)
	}
	if code, _, e := call(nil, "explode"); code != exitError || !strings.Contains(e, "unknown command") {
		t.Errorf("unknown: %d %q", code, e)
	}
}

func TestAnalyzeExitCodes(t *testing.T) {
	code, out, _ := call(nil, "analyze", "-stats", ex+"stats.json", ex+"migrations/001_risky.sql")
	if code != exitDanger || strings.Count(out, "DANGER") != 5 || !strings.Contains(out, "result: danger") {
		t.Errorf("risky: exit %d\n%s", code, out)
	}
	code, out, _ = call(nil, "analyze", "-stats", ex+"stats.json", ex+"migrations/002_safe.sql", ex+"migrations/003_index.sql")
	if code != exitOK || strings.Contains(out, "DANGER") || !strings.Contains(out, "result: ok") {
		t.Errorf("safe: exit %d\n%s", code, out)
	}
	// Without stats the type change cannot be proven safe, so it is flagged.
	if code, _, _ := call(nil, "analyze", ex+"migrations/002_safe.sql"); code != exitDanger {
		t.Errorf("safe file without stats: exit %d, want %d", code, exitDanger)
	}
	// Above -max-rows the table counts as small: rewrites and scans are no longer dangerous.
	if code, out, _ := call(nil, "analyze", "-max-rows", "1000000000", "-stats", ex+"stats.json", ex+"migrations/001_risky.sql"); code != exitOK || strings.Contains(out, "DANGER") {
		t.Errorf("-max-rows above the table size: exit %d\n%s", code, out)
	}
}

func TestAnalyzeJSON(t *testing.T) {
	code, out, _ := call(nil, "analyze", "-json", "-stats", ex+"stats.json", ex+"migrations/001_risky.sql")
	var got []struct {
		File     string `json:"file"`
		Worst    string `json:"worst"`
		Findings []struct {
			Line int    `json:"line"`
			Lock string `json:"lock"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != exitDanger {
		t.Fatalf("exit %d, %v\n%s", code, err, out)
	}
	if len(got) != 1 || got[0].Worst != "danger" || len(got[0].Findings) != 5 || got[0].Findings[2].Lock != "SHARE" || got[0].Findings[0].Line != 2 {
		t.Errorf("unexpected JSON: %+v", got)
	}
}

func TestAnalyzeBadInput(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.sql")
	if err := os.WriteFile(broken, []byte("SELECT 'open"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no files":       {"analyze"},
		"missing file":   {"analyze", filepath.Join(dir, "none.sql")},
		"broken sql":     {"analyze", broken},
		"bad flag":       {"analyze", "-nope", broken},
		"max-rows zero":  {"analyze", "-max-rows", "0", broken},
		"missing stats":  {"analyze", "-stats", filepath.Join(dir, "none.json"), broken},
		"stats not json": {"analyze", "-stats", broken, ex + "migrations/002_safe.sql"},
	} {
		if code, _, e := call(nil, args...); code != exitError || e == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, e)
		}
	}
}

func TestPlanCommand(t *testing.T) {
	code, out, _ := call(nil, "plan", ex+"email_ci.json")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"-- phase expand", "-- phase backfill", "-- phase switch", "-- phase contract",
		"SET LOCAL lock_timeout = '2000ms'", "VALIDATE CONSTRAINT", "worst warn"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q", want)
		}
	}
	code, out, _ = call(nil, "plan", "-json", ex+"rename.json")
	var p struct {
		Phases []struct{ Name string } `json:"phases"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil || code != exitOK || len(p.Phases) != 4 {
		t.Errorf("plan -json: %d %v", code, err)
	}
	for name, args := range map[string][]string{
		"no spec":    {"plan"},
		"two specs":  {"plan", ex + "rename.json", ex + "rename.json"},
		"not a spec": {"plan", ex + "stats.json"},
		"bad flag":   {"plan", "-x", ex + "rename.json"},
	} {
		if code, _, _ := call(nil, args...); code != exitError {
			t.Errorf("%s: exit %d", name, code)
		}
	}
}

func TestRunNeedsDatabaseAndValidFlags(t *testing.T) {
	if code, _, e := call(nil, "run", "-phase", "expand", ex+"rename.json"); code != exitError || !strings.Contains(e, envDSN) {
		t.Errorf("no DSN: %d %q", code, e)
	}
	if code, _, e := call(nil, "status", ex+"rename.json"); code != exitError || !strings.Contains(e, envDSN) {
		t.Errorf("status no DSN: %d %q", code, e)
	}
	if code, _, _ := call(map[string]string{envDSN: "::not a url::"}, "run", "-phase", "expand", ex+"rename.json"); code != exitError {
		t.Error("bad DSN accepted")
	}
	for _, args := range [][]string{
		{"run", "-retries", "-1", ex + "rename.json"},
		{"run", "-retries", "99", ex + "rename.json"},
		{"run", "-statement-timeout", "0s", ex + "rename.json"},
		{"run", "-max-batches", "-2", ex + "rename.json"},
		{"run", "-phase", "expand"},
	} {
		if code, _, _ := call(nil, args...); code != exitError {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// TestPGCommandLineEndToEnd drives every phase through the CLI against a real server.
func TestPGCommandLineEndToEnd(t *testing.T) {
	db := pgtest.New(t)
	c := db.Conn()
	db.Exec(c, `CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL)`)
	db.Exec(c, `INSERT INTO users SELECT g, 'A' || g || '@B.C' FROM generate_series(1, 2500) g`)
	env := map[string]string{envDSN: os.Getenv(pgtest.EnvURL) + "&search_path=" + db.Schema}
	if !strings.Contains(os.Getenv(pgtest.EnvURL), "?") {
		env[envDSN] = os.Getenv(pgtest.EnvURL) + "?search_path=" + db.Schema
	}
	spec := ex + "email_ci.json"
	if _, o, _ := call(env, "status", spec); !strings.Contains(o, "not started") {
		t.Fatalf("status before: %q", o)
	}
	for _, ph := range []string{"expand", "backfill", "switch", "contract"} {
		code, o, e := call(env, "run", "-phase", ph, spec)
		if code != exitOK || !strings.Contains(o, "phase "+ph+":") {
			t.Fatalf("%s: exit %d\n%s\n%s", ph, code, o, e)
		}
		if ph == "backfill" && !strings.Contains(o, "batches=3 rows=2500") {
			t.Errorf("backfill output: %s", o)
		}
	}
	if _, o, _ := call(env, "status", spec); !strings.Contains(o, "users_email_ci: done") {
		t.Errorf("status after: %q", o)
	}
	if n := db.Int(c, `SELECT count(*) FROM users WHERE email_ci = lower('A' || id || '@B.C')`); n != 2500 {
		t.Errorf("%d rows migrated, want 2500", n)
	}
	if code, _, e := call(env, "run", "-phase", "rollback", spec); code != exitError || !strings.Contains(e, "contract has started") {
		t.Errorf("rollback after contract: %d %q", code, e)
	}
}
