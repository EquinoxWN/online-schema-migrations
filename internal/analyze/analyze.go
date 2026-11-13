package analyze

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/EquinoxWN/online-schema-migrations/internal/sqlscan"
)

// Risk is the verdict for one finding.
type Risk int

// Risk levels, least severe first.
const (
	OK Risk = iota
	Warn
	Danger
)

// String returns the lower-case risk name.
func (r Risk) String() string {
	switch r {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Danger:
		return "danger"
	}
	return fmt.Sprintf("Risk(%d)", int(r))
}

// MarshalJSON writes the risk as its name.
func (r Risk) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// DefaultMaxRows is the row count from which a rewrite or scan under a blocking lock is flagged.
const DefaultMaxRows = 100_000

// TableStats describes one existing table.
type TableStats struct {
	Rows    int64             `json:"rows"`
	Columns map[string]string `json:"columns,omitempty"` // column name -> current type
}

// Options tunes the analysis.
type Options struct {
	Stats   map[string]TableStats // keyed by table name, optionally schema-qualified
	MaxRows int64                 // 0 means DefaultMaxRows
}

// Finding is the predicted effect of one statement (or one ALTER TABLE action).
type Finding struct {
	Line         int      `json:"line"`
	Statement    string   `json:"statement"`
	Op           string   `json:"op"`
	Table        string   `json:"table,omitempty"`
	Referenced   string   `json:"referenced,omitempty"` // second table locked (foreign keys)
	Lock         LockMode `json:"lock"`
	Rewrite      bool     `json:"rewrite"`
	Scan         bool     `json:"scan"`
	BlocksReads  bool     `json:"blocks_reads"`
	BlocksWrites bool     `json:"blocks_writes"`
	Risk         Risk     `json:"risk"`
	Reasons      []string `json:"reasons,omitempty"`
	Advice       string   `json:"advice,omitempty"`
}

// LoadStats reads a stats file: {"tables": {"name": {"rows": N, "columns": {...}}}}.
func LoadStats(path string) (map[string]TableStats, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied CLI argument
	if err != nil {
		return nil, fmt.Errorf("read stats: %w", err)
	}
	return ParseStats(raw)
}

// ParseStats decodes and validates a stats document.
func ParseStats(raw []byte) (map[string]TableStats, error) {
	var doc struct {
		Tables map[string]TableStats `json:"tables"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse stats: %w", err)
	}
	if doc.Tables == nil {
		return nil, fmt.Errorf("parse stats: missing \"tables\" object")
	}
	for name, t := range doc.Tables {
		if strings.TrimSpace(name) == "" || t.Rows < 0 {
			return nil, fmt.Errorf("parse stats: table %q has an empty name or negative rows", name)
		}
	}
	return doc.Tables, nil
}

// Worst returns the most severe risk among findings.
func Worst(fs []Finding) Risk {
	w := OK
	for _, f := range fs {
		if f.Risk > w {
			w = f.Risk
		}
	}
	return w
}

// Analyze predicts locks and risks for every statement in a migration file.
func Analyze(src string, opt Options) ([]Finding, error) {
	stmts, err := sqlscan.Split(src)
	if err != nil {
		return nil, err
	}
	if opt.MaxRows <= 0 {
		opt.MaxRows = DefaultMaxRows
	}
	a := &analyzer{opt: opt, created: map[string]bool{}, checks: map[string]string{}, proven: map[string]bool{}}
	var out []Finding
	for _, st := range stmts {
		toks, err := sqlscan.Tokens(st.Text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", st.Line, err)
		}
		for _, f := range a.statement(toks) {
			j := a.judge(f)
			j.Line, j.Statement = st.Line, shorten(st.Text)
			out = append(out, j)
		}
	}
	return out, nil
}

// analyzer carries per-file state between statements.
type analyzer struct {
	opt          Options
	created      map[string]bool   // tables created earlier in this file
	checks       map[string]string // "table/constraint" -> column, for CHECK (col IS NOT NULL)
	proven       map[string]bool   // "table/column" proven NOT NULL by a validated check
	timeout      bool              // session lock_timeout is set
	localTimeout bool              // SET LOCAL lock_timeout in the current transaction
	inTx         bool              // inside BEGIN ... COMMIT
}

// finding is a Finding plus analysis-only flags.
type finding struct {
	Finding
	fullDML   bool // UPDATE/DELETE without WHERE
	failsRows bool // statement fails on a non-empty table
	newTable  bool // CREATE TABLE
	unknown   bool // statement not recognised
	inTxError bool // CONCURRENTLY inside a transaction block
	dataLoss  bool // drops data or a name readers may use
}

// statement dispatches on the first keyword.
func (a *analyzer) statement(t []sqlscan.Token) []finding {
	if len(t) == 0 {
		return nil
	}
	p := &parser{t: t}
	switch {
	case p.accept("begin") || p.accept("start", "transaction"):
		a.inTx = true
		return nil
	case p.accept("commit") || p.accept("end") || p.accept("rollback") || p.accept("abort"):
		a.inTx, a.localTimeout = false, false
		return nil
	case p.accept("set"):
		a.set(p)
		return nil
	case p.accept("alter", "table"):
		return a.alterTable(p)
	case p.accept("create"):
		return a.create(p)
	case p.accept("drop"):
		return a.drop(p)
	case p.accept("truncate"):
		p.accept("table")
		p.accept("only")
		return one(finding{Finding: Finding{Op: "TRUNCATE", Table: p.name(), Lock: AccessExclusive,
			Advice: "deletes every row; make sure nothing still reads this table"}, dataLoss: true})
	case p.accept("vacuum"):
		return one(a.vacuum(p))
	case p.accept("cluster"):
		p.accept("verbose")
		return one(finding{Finding: Finding{Op: "CLUSTER", Table: p.name(), Lock: AccessExclusive, Rewrite: true,
			Advice: "rewrites the table under ACCESS EXCLUSIVE; rebuild online (pg_repack) instead"}})
	case p.accept("reindex"):
		return one(a.reindex(p))
	case p.accept("update"):
		return one(a.dml(p, "UPDATE"))
	case p.accept("delete", "from"):
		return one(a.dml(p, "DELETE"))
	case p.accept("insert", "into"):
		return one(finding{Finding: Finding{Op: "INSERT", Table: p.name(), Lock: RowExclusive}})
	case p.accept("lock"):
		return one(a.lock(p))
	case p.accept("select"), p.accept("with"), p.accept("comment"), p.accept("grant"), p.accept("revoke"), p.accept("analyze"):
		return one(finding{Finding: Finding{Op: strings.ToUpper(t[0].Text)}})
	}
	return one(finding{Finding: Finding{Op: strings.ToUpper(t[0].Text)}, unknown: true})
}

// one wraps a single finding.
func one(f finding) []finding { return []finding{f} }

// set tracks lock_timeout settings.
func (a *analyzer) set(p *parser) {
	local := p.accept("local")
	p.accept("session")
	if !p.accept("lock_timeout") || !(p.accept("=") || p.accept("to")) {
		return
	}
	tok := p.next()
	v := strings.Trim(tok.Text, "'")
	on := v != "0" && v != "" && !tok.Is("default")
	if local {
		a.localTimeout = on
	} else {
		a.timeout = on
	}
}

// judge turns a parsed statement into a verdict, using row counts and lock_timeout state.
func (a *analyzer) judge(f finding) Finding {
	out := f.Finding
	out.BlocksReads, out.BlocksWrites = out.Lock.BlocksReads(), out.Lock.BlocksWrites()
	raise := func(r Risk, reason string) {
		if r > out.Risk {
			out.Risk = r
		}
		out.Reasons = append(out.Reasons, reason)
	}
	if f.unknown {
		raise(Warn, "statement not recognised by the analyser; check its locks by hand")
	}
	if f.inTxError {
		raise(Danger, "CONCURRENTLY cannot run inside a transaction block; PostgreSQL rejects it")
	}
	if f.dataLoss {
		raise(Warn, "removes data or a name that running code may still use; do it only in the contract phase")
	}
	if f.newTable || (out.Table != "" && a.created[out.Table]) {
		out.Reasons = append(out.Reasons, "table created in this migration, so it is empty and not yet used")
		out.Rewrite, out.Scan, out.BlocksReads, out.BlocksWrites = false, false, false, false
		return out
	}
	if f.failsRows {
		if rows, known := a.rows(out.Table); !known || rows > 0 {
			raise(Danger, "NOT NULL column without a default fails on a table that has rows")
			out.Advice = "add the column as nullable, backfill it, then add NOT NULL via CHECK ... NOT VALID and VALIDATE"
		}
	}
	if f.fullDML {
		out.Scan = true
		if a.large(out.Table, &out) {
			raise(Danger, "changes every row in one transaction: row locks on the whole table until commit, and table bloat")
			out.Advice = "update in small batches by primary-key range, committing each batch"
		}
		return out
	}
	blocking := out.BlocksReads || out.BlocksWrites
	if blocking && (out.Rewrite || out.Scan) {
		large := a.large(out.Table, &out)
		if out.Referenced != "" && !a.created[out.Referenced] && a.large(out.Referenced, &out) {
			large = true
		}
		if large {
			what := "scans"
			if out.Rewrite {
				what = "rewrites"
			}
			raise(Danger, fmt.Sprintf("%s the table while holding %s, which blocks %s", what, out.Lock, blocked(out)))
			if out.Advice == "" {
				out.Advice = "use an expand/contract plan (osm plan) or the online form of this statement"
			}
			return out
		}
	}
	if blocking && !a.timeout && !a.localTimeout && out.Risk < Warn {
		raise(Warn, fmt.Sprintf("waits for %s with no lock_timeout: behind a long query, every later query on the table queues behind it", out.Lock))
		if out.Advice == "" {
			out.Advice = "SET lock_timeout = '2s' first and retry on timeout"
		}
	}
	return out
}

// blocked names what a lock blocks.
func blocked(f Finding) string {
	if f.BlocksReads {
		return "reads and writes"
	}
	return "writes"
}

// rows returns a table's row count and whether it is known.
func (a *analyzer) rows(table string) (int64, bool) {
	s, ok := a.lookup(table)
	return s.Rows, ok
}

// lookup finds stats by exact or unqualified name.
func (a *analyzer) lookup(table string) (TableStats, bool) {
	if s, ok := a.opt.Stats[table]; ok {
		return s, true
	}
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		s, ok := a.opt.Stats[table[i+1:]]
		return s, ok
	}
	return TableStats{}, false
}

// large reports a table at or over MaxRows; unknown tables count as large.
func (a *analyzer) large(table string, f *Finding) bool {
	rows, known := a.rows(table)
	if !known {
		if table != "" {
			f.Reasons = append(f.Reasons, fmt.Sprintf("row count of %s unknown, treated as large", table))
		}
		return true
	}
	return rows >= a.opt.MaxRows
}

// columnType returns a column's current type from the stats file.
func (a *analyzer) columnType(table, col string) string {
	s, _ := a.lookup(table)
	return s.Columns[col]
}

// shorten makes a one-line excerpt of a statement.
func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
