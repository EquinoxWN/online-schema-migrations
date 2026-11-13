package analyze

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/EquinoxWN/online-schema-migrations/internal/sqlscan"
)

// parser walks a token slice.
type parser struct {
	t []sqlscan.Token
	i int
}

// peek returns the current token, or an empty one at the end.
func (p *parser) peek() sqlscan.Token {
	if p.i < len(p.t) {
		return p.t[p.i]
	}
	return sqlscan.Token{Kind: sqlscan.Punct}
}

// next consumes and returns the current token.
func (p *parser) next() sqlscan.Token {
	tok := p.peek()
	if p.i < len(p.t) {
		p.i++
	}
	return tok
}

// accept consumes the given keyword or punctuation sequence if it is next.
func (p *parser) accept(words ...string) bool {
	if p.i+len(words) > len(p.t) {
		return false
	}
	for k, w := range words {
		tok := p.t[p.i+k]
		if !tok.Is(w) && !(tok.Kind == sqlscan.Punct && tok.Text == w) {
			return false
		}
	}
	p.i += len(words)
	return true
}

// name reads an optionally schema-qualified name.
func (p *parser) name() string {
	tok := p.peek()
	if tok.Kind != sqlscan.Word && tok.Kind != sqlscan.Ident {
		return ""
	}
	p.i++
	n := tok.Text
	if p.accept(".") {
		if t := p.peek(); t.Kind == sqlscan.Word || t.Kind == sqlscan.Ident {
			p.i++
			n += "." + t.Text
		}
	}
	return n
}

// rest returns the remaining tokens.
func (p *parser) rest() []sqlscan.Token { return p.t[p.i:] }

// splitTop cuts tokens at top-level commas.
func splitTop(t []sqlscan.Token) [][]sqlscan.Token {
	var out [][]sqlscan.Token
	depth, start := 0, 0
	for i, tok := range t {
		if tok.Kind != sqlscan.Punct {
			continue
		}
		switch tok.Text {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 0 {
				out = append(out, t[start:i])
				start = i + 1
			}
		}
	}
	return append(out, t[start:])
}

// hasTop reports a keyword outside parentheses.
func hasTop(t []sqlscan.Token, word string) bool {
	depth := 0
	for _, tok := range t {
		switch {
		case tok.Kind == sqlscan.Punct && tok.Text == "(":
			depth++
		case tok.Kind == sqlscan.Punct && tok.Text == ")":
			depth--
		case depth == 0 && tok.Is(word):
			return true
		}
	}
	return false
}

// text joins tokens back into SQL-ish text.
func text(t []sqlscan.Token) string {
	var b strings.Builder
	for i, tok := range t {
		if i > 0 && tok.Kind != sqlscan.Punct && t[i-1].Text != "(" {
			b.WriteByte(' ')
		}
		b.WriteString(tok.Text)
	}
	return b.String()
}

// alterTable handles ALTER TABLE; each action becomes one finding.
func (a *analyzer) alterTable(p *parser) []finding {
	p.accept("if", "exists")
	p.accept("only")
	table := p.name()
	p.accept("*")
	if p.accept("rename") {
		f := finding{Finding: Finding{Table: table, Lock: AccessExclusive}, dataLoss: true}
		switch {
		case p.accept("to"):
			f.Op = "ALTER TABLE RENAME TO"
		case p.accept("constraint"):
			f.Op, f.dataLoss = "ALTER TABLE RENAME CONSTRAINT", false
		default:
			f.Op = "ALTER TABLE RENAME COLUMN"
			f.Advice = "renaming breaks code that uses the old name; add a new column and follow expand/contract instead"
		}
		return one(f)
	}
	var out []finding
	for _, act := range splitTop(p.rest()) {
		out = append(out, a.action(table, &parser{t: act}))
	}
	return out
}

// action classifies one ALTER TABLE action.
func (a *analyzer) action(table string, p *parser) finding {
	f := finding{Finding: Finding{Table: table, Lock: AccessExclusive}}
	switch {
	case p.accept("add"):
		if p.accept("constraint") {
			return a.constraint(f, p, p.name())
		}
		if t := p.peek(); t.Is("check") || t.Is("foreign") || t.Is("unique") || t.Is("primary") || t.Is("exclude") {
			return a.constraint(f, p, "")
		}
		p.accept("column")
		p.accept("if", "not", "exists")
		p.name()
		return addColumn(f, p.rest())
	case p.accept("drop"):
		if p.accept("constraint") {
			f.Op = "ALTER TABLE DROP CONSTRAINT"
			return f
		}
		f.Op, f.dataLoss = "ALTER TABLE DROP COLUMN", true
		f.Advice = "only after every reader and writer stopped using the column (contract phase)"
		return f
	case p.accept("alter"):
		p.accept("column")
		return a.alterColumn(f, p.name(), p)
	case p.accept("validate", "constraint"):
		name := p.name()
		f.Op, f.Lock, f.Scan = "ALTER TABLE VALIDATE CONSTRAINT", ShareUpdateExclusive, true
		if col, ok := a.checks[table+"/"+name]; ok {
			a.proven[table+"/"+col] = true
		}
		return f
	case p.accept("set"):
		switch {
		case p.accept("logged"), p.accept("unlogged"), p.accept("tablespace"), p.accept("access", "method"):
			f.Op, f.Rewrite = "ALTER TABLE SET (rewrite)", true
		case p.accept("("):
			f.Op, f.Lock = "ALTER TABLE SET (storage parameters)", ShareUpdateExclusive
		default:
			f.Op = "ALTER TABLE SET"
		}
		return f
	case p.accept("enable"), p.accept("disable"):
		f.Op, f.Lock = "ALTER TABLE ENABLE/DISABLE TRIGGER", ShareRowExclusive
		return f
	case p.accept("attach", "partition"):
		f.Op, f.Lock, f.Scan = "ALTER TABLE ATTACH PARTITION", ShareUpdateExclusive, true
		return f
	case p.accept("detach", "partition"):
		p.name()
		f.Op = "ALTER TABLE DETACH PARTITION"
		if p.accept("concurrently") {
			f.Lock = ShareUpdateExclusive
			f.inTxError = a.inTx
		}
		return f
	case p.accept("owner", "to"):
		f.Op = "ALTER TABLE OWNER TO"
		return f
	}
	f.Op, f.unknown = "ALTER TABLE "+strings.ToUpper(p.peek().Text), true
	return f
}

// volatile lists functions whose value differs per row, forcing a rewrite as a column default.
var volatile = map[string]bool{
	"random": true, "gen_random_uuid": true, "uuid_generate_v1": true, "uuid_generate_v4": true,
	"clock_timestamp": true, "timeofday": true, "nextval": true, "uuidv7": true, "uuidv4": true,
}

// columnStops are keywords that end a column type or default expression.
var columnStops = map[string]bool{
	"default": true, "not": true, "null": true, "check": true, "references": true, "unique": true,
	"primary": true, "constraint": true, "generated": true, "collate": true,
}

// addColumn classifies ADD COLUMN from the tokens after the column name.
func addColumn(f finding, t []sqlscan.Token) finding {
	f.Op = "ALTER TABLE ADD COLUMN"
	typ, i := untilStop(t, 0)
	switch strings.ToLower(text(typ)) {
	case "serial", "bigserial", "smallserial", "serial4", "serial8", "serial2":
		f.Rewrite = true
		f.Reasons = append(f.Reasons, "serial types add a nextval() default, so every row is rewritten")
	}
	hasDefault, notNull := false, false
	for i < len(t) {
		switch tok := t[i]; {
		case tok.Is("default"):
			var expr []sqlscan.Token
			expr, i = untilStop(t, i+1)
			hasDefault = len(expr) > 0 // DEFAULT NULL leaves expr empty: "null" ends it
			if callsVolatile(expr) {
				f.Rewrite = true
				f.Reasons = append(f.Reasons, "volatile default: PostgreSQL computes it per row and rewrites the table")
				f.Advice = "add the column without a default, set the default afterwards, and backfill in batches"
			}
			continue
		case tok.Is("not") && i+1 < len(t) && t[i+1].Is("null"):
			notNull = true
			i += 2
			continue
		case tok.Is("generated"):
			f.Rewrite = true
			f.Reasons = append(f.Reasons, "generated or identity column: every row is written")
		case tok.Is("check"):
			f.Scan = true
		case tok.Is("references"):
			f.Scan = true
			if i+1 < len(t) {
				f.Referenced = t[i+1].Text
			}
		case tok.Is("unique"), tok.Is("primary"):
			f.Scan = true
			f.Reasons = append(f.Reasons, "inline UNIQUE/PRIMARY KEY builds an index over the whole table")
		}
		i++
	}
	if notNull && !hasDefault && !f.Rewrite {
		f.failsRows = true
	}
	return f
}

// untilStop collects tokens from i up to the next top-level column keyword.
func untilStop(t []sqlscan.Token, i int) ([]sqlscan.Token, int) {
	depth, start := 0, i
	for ; i < len(t); i++ {
		tok := t[i]
		if tok.Kind == sqlscan.Punct && tok.Text == "(" {
			depth++
		}
		if tok.Kind == sqlscan.Punct && tok.Text == ")" {
			depth--
		}
		if depth == 0 && tok.Kind == sqlscan.Word && columnStops[tok.Text] {
			break
		}
	}
	return t[start:i], i
}

// callsVolatile reports a call to a known volatile function.
func callsVolatile(expr []sqlscan.Token) bool {
	for i := 0; i+1 < len(expr); i++ {
		if expr[i].Kind == sqlscan.Word && volatile[expr[i].Text] && expr[i+1].Text == "(" {
			return true
		}
	}
	return false
}

// constraint classifies ADD CONSTRAINT.
func (a *analyzer) constraint(f finding, p *parser, name string) finding {
	notValid := hasTop(p.rest(), "valid") && hasNotValid(p.rest())
	switch {
	case p.accept("check"):
		f.Op = "ALTER TABLE ADD CHECK"
		f.Scan = !notValid
		if col := notNullCheck(p.rest()); col != "" {
			if name != "" {
				a.checks[f.Table+"/"+name] = col
			}
			if !notValid {
				a.proven[f.Table+"/"+col] = true
			}
		}
	case p.accept("foreign", "key"):
		f.Op, f.Lock, f.Scan = "ALTER TABLE ADD FOREIGN KEY", ShareRowExclusive, !notValid
		rest := p.rest()
		for i, tok := range rest {
			if tok.Is("references") && i+1 < len(rest) {
				f.Referenced = (&parser{t: rest[i+1:]}).name()
			}
		}
	case p.accept("unique"), p.accept("primary", "key"):
		f.Op = "ALTER TABLE ADD UNIQUE/PRIMARY KEY"
		if hasTop(p.rest(), "using") {
			f.Op += " USING INDEX"
		} else {
			f.Scan = true
			f.Advice = "CREATE UNIQUE INDEX CONCURRENTLY first, then ADD CONSTRAINT ... USING INDEX"
		}
	case p.accept("exclude"):
		f.Op, f.Scan = "ALTER TABLE ADD EXCLUDE", true
	default:
		f.Op, f.unknown = "ALTER TABLE ADD CONSTRAINT", true
	}
	if f.Scan && f.Advice == "" {
		f.Advice = "add it NOT VALID (brief lock), then VALIDATE CONSTRAINT (does not block reads or writes)"
	}
	return f
}

// hasNotValid reports a top-level NOT VALID.
func hasNotValid(t []sqlscan.Token) bool {
	for i := 0; i+1 < len(t); i++ {
		if t[i].Is("not") && t[i+1].Is("valid") {
			return true
		}
	}
	return false
}

// notNullCheck returns col for CHECK (col IS NOT NULL), else "".
func notNullCheck(t []sqlscan.Token) string {
	if len(t) >= 6 && t[0].Text == "(" && t[2].Is("is") && t[3].Is("not") && t[4].Is("null") && t[5].Text == ")" {
		return t[1].Text
	}
	return ""
}

// alterColumn classifies ALTER COLUMN actions.
func (a *analyzer) alterColumn(f finding, col string, p *parser) finding {
	switch {
	case p.accept("type"), p.accept("set", "data", "type"):
		f.Op = "ALTER TABLE ALTER COLUMN TYPE"
		typ, _ := untilWord(p.rest(), "using", "collate")
		newType := text(typ)
		old := a.columnType(f.Table, col)
		switch {
		case hasTop(p.rest(), "using"):
			f.Rewrite = true
			f.Reasons = append(f.Reasons, "USING expression: every row is recomputed")
		case old == "":
			f.Rewrite = true
			f.Reasons = append(f.Reasons, "current type of "+col+" unknown (add it to the stats file); assumed to rewrite")
		case BinaryCoercible(old, newType):
			f.Reasons = append(f.Reasons, old+" -> "+newType+" is binary coercible: no rewrite")
		default:
			f.Rewrite = true
			f.Reasons = append(f.Reasons, old+" -> "+newType+" changes the on-disk format: every row is rewritten")
		}
		if f.Rewrite {
			f.Advice = "add a new column and follow expand/contract (osm plan) instead of changing the type in place"
		}
	case p.accept("set", "not", "null"):
		f.Op = "ALTER TABLE ALTER COLUMN SET NOT NULL"
		if a.proven[f.Table+"/"+col] {
			f.Reasons = append(f.Reasons, "a validated CHECK ("+col+" IS NOT NULL) lets PostgreSQL 12+ skip the scan")
		} else {
			f.Scan = true
			f.Advice = "ADD CONSTRAINT ... CHECK (" + col + " IS NOT NULL) NOT VALID, VALIDATE it, then SET NOT NULL"
		}
	case p.accept("drop", "not", "null"):
		f.Op = "ALTER TABLE ALTER COLUMN DROP NOT NULL"
	case p.accept("set", "default"):
		f.Op = "ALTER TABLE ALTER COLUMN SET DEFAULT"
	case p.accept("drop", "default"):
		f.Op = "ALTER TABLE ALTER COLUMN DROP DEFAULT"
	case p.accept("set", "statistics"):
		f.Op, f.Lock = "ALTER TABLE ALTER COLUMN SET STATISTICS", ShareUpdateExclusive
	default:
		f.Op, f.unknown = "ALTER TABLE ALTER COLUMN "+strings.ToUpper(p.peek().Text), true
	}
	return f
}

// untilWord collects tokens up to the first top-level keyword in words.
func untilWord(t []sqlscan.Token, words ...string) ([]sqlscan.Token, int) {
	depth := 0
	for i, tok := range t {
		if tok.Kind == sqlscan.Punct && tok.Text == "(" {
			depth++
		}
		if tok.Kind == sqlscan.Punct && tok.Text == ")" {
			depth--
		}
		for _, w := range words {
			if depth == 0 && tok.Is(w) {
				return t[:i], i
			}
		}
	}
	return t, len(t)
}

var typeRe = regexp.MustCompile(`^([a-z ]+?)\s*(?:\(\s*(\d+)\s*(?:,\s*(\d+)\s*)?\))?$`)

// normType splits a type into base name and modifiers (-1 when absent).
func normType(s string) (string, int, int) {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	m := typeRe.FindStringSubmatch(s)
	if m == nil {
		return s, -1, -1
	}
	base := strings.TrimSpace(m[1])
	if base == "character varying" {
		base = "varchar"
	}
	p, sc := -1, -1
	if m[2] != "" {
		p, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		sc, _ = strconv.Atoi(m[3])
	}
	if base == "numeric" || base == "decimal" {
		base = "numeric"
		if p >= 0 && sc < 0 {
			sc = 0
		}
	}
	return base, p, sc
}

// BinaryCoercible reports type changes PostgreSQL applies without rewriting the table.
func BinaryCoercible(from, to string) bool {
	fb, fp, fs := normType(from)
	tb, tp, ts := normType(to)
	switch {
	case fb == tb && fp == tp && fs == ts:
		return true
	case fb == "varchar" && tb == "text":
		return true
	case fb == "text" && tb == "varchar" && tp < 0:
		return true
	case fb == "varchar" && tb == "varchar":
		return tp < 0 || fp >= 0 && tp >= fp
	case fb == "numeric" && tb == "numeric":
		return tp < 0 || fp >= 0 && tp >= fp && ts == fs
	}
	return false
}

// create handles CREATE TABLE, INDEX and TRIGGER.
func (a *analyzer) create(p *parser) []finding {
	p.accept("or", "replace")
	unique := p.accept("unique")
	for p.accept("temp") || p.accept("temporary") || p.accept("unlogged") || p.accept("global") || p.accept("local") {
	}
	switch {
	case p.accept("table"):
		p.accept("if", "not", "exists")
		t := p.name()
		a.created[t] = true
		return one(finding{Finding: Finding{Op: "CREATE TABLE", Table: t, Lock: AccessExclusive}, newTable: true})
	case p.accept("index"):
		f := finding{Finding: Finding{Op: "CREATE INDEX", Lock: Share, Scan: true}}
		if unique {
			f.Op = "CREATE UNIQUE INDEX"
		}
		if p.accept("concurrently") {
			f.Op += " CONCURRENTLY"
			f.Lock = ShareUpdateExclusive
			f.inTxError = a.inTx
		} else {
			f.Advice = "use CREATE INDEX CONCURRENTLY (outside a transaction block)"
		}
		p.accept("if", "not", "exists")
		if !p.peek().Is("on") {
			p.name()
		}
		p.accept("on")
		p.accept("only")
		f.Table = p.name()
		return one(f)
	case p.accept("trigger"), p.accept("constraint", "trigger"):
		p.name()
		for p.i < len(p.t) && !p.peek().Is("on") {
			p.next()
		}
		p.accept("on")
		return one(finding{Finding: Finding{Op: "CREATE TRIGGER", Table: p.name(), Lock: ShareRowExclusive}})
	case p.accept("function"), p.accept("procedure"), p.accept("view"), p.accept("type"), p.accept("extension"),
		p.accept("sequence"), p.accept("schema"), p.accept("domain"), p.accept("materialized", "view"):
		return one(finding{Finding: Finding{Op: "CREATE " + strings.ToUpper(p.t[p.i-1].Text)}})
	}
	return one(finding{Finding: Finding{Op: "CREATE " + strings.ToUpper(p.peek().Text)}, unknown: true})
}

// drop handles DROP TABLE, INDEX, TRIGGER and others.
func (a *analyzer) drop(p *parser) []finding {
	switch {
	case p.accept("table"):
		p.accept("if", "exists")
		return one(finding{Finding: Finding{Op: "DROP TABLE", Table: p.name(), Lock: AccessExclusive}, dataLoss: true})
	case p.accept("index"):
		f := finding{Finding: Finding{Op: "DROP INDEX", Lock: AccessExclusive}}
		if p.accept("concurrently") {
			f.Op, f.Lock, f.inTxError = "DROP INDEX CONCURRENTLY", ShareUpdateExclusive, a.inTx
		} else {
			f.Advice = "use DROP INDEX CONCURRENTLY; the plain form takes ACCESS EXCLUSIVE on the index's table"
		}
		p.accept("if", "exists")
		p.name()
		return one(f)
	case p.accept("trigger"):
		p.accept("if", "exists")
		p.name()
		p.accept("on")
		return one(finding{Finding: Finding{Op: "DROP TRIGGER", Table: p.name(), Lock: AccessExclusive}})
	case p.accept("function"), p.accept("procedure"), p.accept("view"), p.accept("type"), p.accept("sequence"):
		return one(finding{Finding: Finding{Op: "DROP " + strings.ToUpper(p.t[p.i-1].Text)}})
	}
	return one(finding{Finding: Finding{Op: "DROP " + strings.ToUpper(p.peek().Text)}, unknown: true})
}

// vacuum handles VACUUM; only FULL rewrites.
func (a *analyzer) vacuum(p *parser) finding {
	full := false
	if p.accept("(") {
		for p.i < len(p.t) && !p.accept(")") {
			if p.next().Is("full") && !p.peek().Is("false") && !p.peek().Is("off") {
				full = true
			}
		}
	}
	for p.peek().Is("full") || p.peek().Is("freeze") || p.peek().Is("verbose") || p.peek().Is("analyze") {
		if p.next().Is("full") {
			full = true
		}
	}
	f := finding{Finding: Finding{Op: "VACUUM", Table: p.name(), Lock: ShareUpdateExclusive}}
	if full {
		f.Op, f.Lock, f.Rewrite = "VACUUM FULL", AccessExclusive, true
		f.Advice = "rebuild online (pg_repack) instead of VACUUM FULL"
	}
	return f
}

// reindex handles REINDEX.
func (a *analyzer) reindex(p *parser) finding {
	f := finding{Finding: Finding{Op: "REINDEX", Lock: Share, Scan: true}}
	isTable := p.accept("table")
	if !isTable {
		p.accept("index")
	}
	if p.accept("concurrently") {
		f.Op, f.Lock, f.inTxError = "REINDEX CONCURRENTLY", ShareUpdateExclusive, a.inTx
	} else {
		f.Advice = "use REINDEX ... CONCURRENTLY"
	}
	n := p.name()
	if isTable {
		f.Table = n
	}
	return f
}

// dml handles UPDATE and DELETE.
func (a *analyzer) dml(p *parser, op string) finding {
	p.accept("only")
	f := finding{Finding: Finding{Op: op, Table: p.name(), Lock: RowExclusive}}
	f.fullDML = !hasTop(p.rest(), "where")
	return f
}

// lockModes maps LOCK ... IN <words> MODE to a lock mode.
var lockModes = map[string]LockMode{
	"access share": AccessShare, "row share": RowShare, "row exclusive": RowExclusive,
	"share update exclusive": ShareUpdateExclusive, "share": Share, "share row exclusive": ShareRowExclusive,
	"exclusive": Exclusive, "access exclusive": AccessExclusive,
}

// lock handles LOCK TABLE.
func (a *analyzer) lock(p *parser) finding {
	p.accept("table")
	p.accept("only")
	f := finding{Finding: Finding{Op: "LOCK TABLE", Table: p.name(), Lock: AccessExclusive}}
	if p.accept("in") {
		var words []string
		for p.i < len(p.t) && !p.peek().Is("mode") {
			words = append(words, p.next().Text)
		}
		if m, ok := lockModes[strings.Join(words, " ")]; ok {
			f.Lock = m
		} else {
			f.unknown = true
		}
	}
	return f
}
