// Package plan turns a column change into expand/contract phases with their SQL and rollback.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/EquinoxWN/online-schema-migrations/internal/sqlscan"
)

// Change kinds.
const (
	RenameColumn = "rename_column"
	ChangeType   = "change_type"
)

// Limits on spec values.
const (
	DefaultBatch       = 1000
	MaxBatch           = 100_000
	DefaultLockTimeout = 2 * time.Second
	maxExpr            = 1000
)

// Spec describes one column change; it is trusted like migration code.
type Spec struct {
	ID          string `json:"id"`
	Table       string `json:"table"`
	Key         string `json:"key"`
	Kind        string `json:"kind"`
	From        string `json:"from"`
	To          string `json:"to"`
	Type        string `json:"type"`
	Up          string `json:"up,omitempty"`
	Down        string `json:"down,omitempty"`
	NotNull     bool   `json:"not_null,omitempty"`
	BatchSize   int    `json:"batch_size,omitempty"`
	LockTimeout string `json:"lock_timeout,omitempty"`
}

var (
	identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	idRe    = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	typeRe  = regexp.MustCompile(`^[a-z][a-z0-9_ ]{0,62}(\(\s*\d+\s*(,\s*\d+\s*)?\))?(\[\])?$`)
)

// ParseSpec decodes a spec strictly and validates it.
func ParseSpec(raw []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	if dec.More() {
		return Spec{}, errors.New("spec: trailing data after the JSON object")
	}
	if err := s.Normalize(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// Normalize fills defaults and rejects invalid specs.
func (s *Spec) Normalize() error {
	if !idRe.MatchString(s.ID) {
		return fmt.Errorf("spec: id %q must be 1-40 characters of a-z, 0-9 and _", s.ID)
	}
	for _, f := range []struct{ name, v string }{{"table", s.Table}, {"key", s.Key}, {"from", s.From}, {"to", s.To}} {
		if !identRe.MatchString(f.v) {
			return fmt.Errorf("spec: %s %q must be a lower-case identifier (a-z, 0-9, _; max 63)", f.name, f.v)
		}
	}
	if s.From == s.To || s.Key == s.From || s.Key == s.To {
		return errors.New("spec: from, to and key must be three different columns")
	}
	s.Type = strings.ToLower(strings.TrimSpace(s.Type))
	if !typeRe.MatchString(s.Type) {
		return fmt.Errorf("spec: type %q is not a plain type name such as text, bigint or varchar(200)", s.Type)
	}
	switch s.Kind {
	case RenameColumn:
		if s.Up != "" || s.Down != "" {
			return errors.New("spec: rename_column copies values as they are; up and down must be empty")
		}
		s.Up, s.Down = quote(s.From), quote(s.To)
	case ChangeType:
		if s.Up == "" || s.Down == "" {
			return errors.New("spec: change_type needs up (new from old) and down (old from new) expressions")
		}
		for _, e := range []string{s.Up, s.Down} {
			if err := checkExpr(e); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("spec: kind %q must be %s or %s", s.Kind, RenameColumn, ChangeType)
	}
	if s.BatchSize == 0 {
		s.BatchSize = DefaultBatch
	}
	if s.BatchSize < 1 || s.BatchSize > MaxBatch {
		return fmt.Errorf("spec: batch_size %d must be between 1 and %d", s.BatchSize, MaxBatch)
	}
	if s.LockTimeout == "" {
		s.LockTimeout = DefaultLockTimeout.String()
	}
	d, err := time.ParseDuration(s.LockTimeout)
	if err != nil || d < 10*time.Millisecond || d > time.Minute {
		return fmt.Errorf("spec: lock_timeout %q must be a duration from 10ms to 1m", s.LockTimeout)
	}
	s.LockTimeout = d.String()
	return nil
}

// checkExpr rejects expressions that could end the statement or the trigger body.
func checkExpr(e string) error {
	if len(e) > maxExpr {
		return fmt.Errorf("spec: expression longer than %d bytes", maxExpr)
	}
	if strings.Contains(e, "$osm$") {
		return errors.New("spec: expression may not contain $osm$")
	}
	toks, err := sqlscan.Tokens(e)
	if err != nil {
		return fmt.Errorf("spec: expression %q: %w", e, err)
	}
	depth := 0
	for _, t := range toks {
		if t.Kind != sqlscan.Punct {
			continue
		}
		switch t.Text {
		case ";":
			return fmt.Errorf("spec: expression %q contains ';'", e)
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return fmt.Errorf("spec: expression %q has unbalanced parentheses", e)
			}
		}
	}
	if depth != 0 || len(toks) == 0 {
		return fmt.Errorf("spec: expression %q is empty or has unbalanced parentheses", e)
	}
	return nil
}

// LockTimeoutDuration returns the parsed lock timeout of a normalized spec.
func (s Spec) LockTimeoutDuration() time.Duration {
	d, err := time.ParseDuration(s.LockTimeout)
	if err != nil {
		return DefaultLockTimeout
	}
	return d
}

// Hash identifies the normalized spec, so a run cannot continue under a changed spec.
func (s Spec) Hash() string {
	b, _ := json.Marshal(s) // a struct of strings, ints and bools always marshals
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// quote double-quotes an identifier already restricted to [a-z0-9_].
func quote(id string) string { return `"` + id + `"` }

// Phase names, in the order they run.
const (
	Expand   = "expand"
	Backfill = "backfill"
	Switch   = "switch"
	Contract = "contract"
)

// Phases lists the phases in order.
var Phases = []string{Expand, Backfill, Switch, Contract}

// Step is a group of statements that runs in one transaction.
type Step struct {
	Name string   `json:"name"`
	SQL  []string `json:"sql"`
}

// Phase is one stage of the change.
type Phase struct {
	Name     string `json:"name"`
	Purpose  string `json:"purpose"`
	Steps    []Step `json:"steps,omitempty"`
	Rollback string `json:"rollback"`
}

// Plan is the full expand/contract sequence for a spec.
type Plan struct {
	Spec   Spec    `json:"spec"`
	Phases []Phase `json:"phases"`
}

// Build produces the plan for a normalized spec.
func Build(s Spec) Plan {
	return Plan{Spec: s, Phases: []Phase{
		{Name: Expand, Purpose: fmt.Sprintf("add %s as a nullable column and keep %s and %s in sync on every write (dual-write)", s.To, s.From, s.To),
			Steps:    []Step{{Name: "add column and sync trigger", SQL: ExpandSQL(s)}},
			Rollback: "osm run -phase rollback: drops the trigger, the function and the new column; the old column is untouched"},
		{Name: Backfill, Purpose: fmt.Sprintf("copy existing rows into %s in batches of %d by %s range, one transaction per batch", s.To, s.BatchSize, s.Key),
			Steps:    []Step{{Name: "batch (repeated)", SQL: []string{BackfillSQL(s)}}},
			Rollback: "osm run -phase rollback, as for expand; batches already written are dropped with the new column"},
		{Name: Switch, Purpose: fmt.Sprintf("check that no row has %s out of sync, then deploy code that reads %s; old and new code both keep working because the trigger still dual-writes", s.To, s.To),
			Steps:    []Step{{Name: "verify no row is out of sync", SQL: []string{PendingSQL(s)}}},
			Rollback: "deploy the previous code again (it still reads " + s.From + ", which the trigger keeps current), or osm run -phase rollback"},
		{Name: Contract, Purpose: fmt.Sprintf("once no running code uses %s, stop dual-writing and drop it", s.From),
			Steps:    ContractSteps(s),
			Rollback: "none: dropping " + s.From + " is the point of no return; take a backup or keep the column longer if unsure"},
	}}
}

// FuncName is the trigger function and trigger name for a spec.
func FuncName(s Spec) string { return "osm_sync_" + s.ID }

// ExpandSQL adds the new column, the sync function and the trigger.
func ExpandSQL(s Spec) []string {
	t, from, to, fn := quote(s.Table), quote(s.From), quote(s.To), quote(FuncName(s))
	body := fmt.Sprintf(`
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.%[1]s IS NULL AND NEW.%[2]s IS NOT NULL THEN
      NEW.%[1]s := (SELECT (%[3]s) FROM (SELECT NEW.*) AS osm_row);
    ELSIF NEW.%[2]s IS NULL AND NEW.%[1]s IS NOT NULL THEN
      NEW.%[2]s := (SELECT (%[4]s) FROM (SELECT NEW.*) AS osm_row);
    END IF;
  ELSIF NEW.%[2]s IS DISTINCT FROM OLD.%[2]s THEN
    NEW.%[1]s := (SELECT (%[3]s) FROM (SELECT NEW.*) AS osm_row);
  ELSIF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s AND current_setting('osm.backfill', true) IS DISTINCT FROM 'on' THEN
    NEW.%[2]s := (SELECT (%[4]s) FROM (SELECT NEW.*) AS osm_row);
  END IF;
  RETURN NEW;
END`, to, from, s.Up, s.Down)
	return []string{
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", t, to, s.Type),
		fmt.Sprintf("CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $osm$%s\n$osm$", fn, body),
		fmt.Sprintf("CREATE OR REPLACE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s()", fn, t, fn),
	}
}

// BackfillSQL updates one key range ($1, $2]; rows already in sync are skipped.
func BackfillSQL(s Spec) string {
	return fmt.Sprintf("UPDATE %s SET %s = (%s) WHERE %s > $1 AND %s <= $2 AND %s IS DISTINCT FROM (%s)",
		quote(s.Table), quote(s.To), s.Up, quote(s.Key), quote(s.Key), quote(s.To), s.Up)
}

// PendingSQL counts rows whose new column does not match the old one.
func PendingSQL(s Spec) string {
	return fmt.Sprintf("SELECT count(*) FROM %s WHERE %s IS DISTINCT FROM (%s)", quote(s.Table), quote(s.To), s.Up)
}

// MaxKeySQL returns the highest key, the end of the backfill range.
func MaxKeySQL(s Spec) string {
	return fmt.Sprintf("SELECT max(%s) FROM %s", quote(s.Key), quote(s.Table))
}

// ContractSteps drops dual-writing and the old column, adding NOT NULL first if asked.
func ContractSteps(s Spec) []Step {
	t, to, fn := quote(s.Table), quote(s.To), quote(FuncName(s))
	var steps []Step
	if s.NotNull {
		c := quote("osm_nn_" + s.ID)
		steps = append(steps,
			Step{Name: "add NOT NULL check without scanning", SQL: []string{fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", t, c, to)}},
			Step{Name: "validate the check (reads and writes continue)", SQL: []string{fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", t, c)}},
			Step{Name: "set NOT NULL using the validated check", SQL: []string{
				fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", t, to),
				fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", t, c),
			}})
	}
	return append(steps, Step{Name: "stop dual-writing and drop the old column", SQL: []string{
		fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", fn, t),
		fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn),
		fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", t, quote(s.From)),
	}})
}

// RollbackSQL undoes expand: drops the trigger, function and new column.
func RollbackSQL(s Spec) []string {
	t, fn := quote(s.Table), quote(FuncName(s))
	return []string{
		fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", fn, t),
		fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn),
		fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", t, quote(s.To)),
	}
}

// Script renders every step with SET LOCAL lock_timeout, for review or for the lock analyser.
func (p Plan) Script() string {
	var b strings.Builder
	for _, ph := range p.Phases {
		fmt.Fprintf(&b, "-- phase %s: %s\n", ph.Name, ph.Purpose)
		for _, st := range ph.Steps {
			fmt.Fprintf(&b, "-- step: %s\nBEGIN;\nSET LOCAL lock_timeout = '%dms';\n", st.Name, p.Spec.LockTimeoutDuration().Milliseconds())
			if ph.Name == Backfill {
				b.WriteString("SET LOCAL osm.backfill = 'on';\n")
			}
			for _, q := range st.SQL {
				if ph.Name == Backfill {
					b.WriteString("-- first batch shown; each batch covers the next key range (lo, hi]\n")
					q = strings.NewReplacer("$1", "0", "$2", fmt.Sprint(p.Spec.BatchSize)).Replace(q)
				}
				b.WriteString(q + ";\n")
			}
			b.WriteString("COMMIT;\n")
		}
		fmt.Fprintf(&b, "-- rollback: %s\n\n", ph.Rollback)
	}
	return b.String()
}
