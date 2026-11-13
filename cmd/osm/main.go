// Command osm analyses migration locks and runs expand/contract column changes on PostgreSQL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EquinoxWN/online-schema-migrations/internal/analyze"
	"github.com/EquinoxWN/online-schema-migrations/internal/plan"
	"github.com/EquinoxWN/online-schema-migrations/internal/run"
	"github.com/EquinoxWN/online-schema-migrations/internal/sqlscan"
)

// Exit codes.
const (
	exitOK     = 0
	exitDanger = 1 // analyze found a dangerous statement
	exitError  = 2 // bad usage, unreadable input or a failed run
)

// envDSN names the variable holding the PostgreSQL connection string for run and status.
const envDSN = "OSM_DATABASE_URL"

const usage = `usage:
  osm analyze [-stats stats.json] [-max-rows N] [-json] migration.sql...
  osm plan [-json] spec.json
  osm run -phase expand|backfill|switch|contract|rollback [flags] spec.json
  osm status spec.json
run and status connect to $OSM_DATABASE_URL.`

// main wires signals and the process exit code.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// cli runs one command and returns the exit code.
func cli(ctx context.Context, args []string, out, errOut io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, usage)
		return exitError
	}
	var err error
	code := exitOK
	switch args[0] {
	case "analyze":
		code, err = cmdAnalyze(args[1:], out)
	case "plan":
		err = cmdPlan(args[1:], out)
	case "run":
		err = cmdRun(ctx, args[1:], out, getenv)
	case "status":
		err = cmdStatus(ctx, args[1:], out, getenv)
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(out, usage)
		return exitOK
	default:
		err = fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
	if err != nil {
		fmt.Fprintln(errOut, "osm:", err)
		return exitError
	}
	return code
}

// newFlags makes a flag set that reports errors instead of exiting.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// readFile reads at most sqlscan.MaxInput bytes.
func readFile(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied CLI argument
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, sqlscan.MaxInput+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > sqlscan.MaxInput {
		return nil, fmt.Errorf("%s: %w", path, sqlscan.ErrTooLarge)
	}
	return b, nil
}

// cmdAnalyze prints findings and returns exitDanger if any statement is dangerous.
func cmdAnalyze(args []string, out io.Writer) (int, error) {
	fs := newFlags("analyze")
	statsPath := fs.String("stats", "", "JSON file with row counts and column types")
	maxRows := fs.Int64("max-rows", analyze.DefaultMaxRows, "row count from which a rewrite or scan under a blocking lock is dangerous")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	if fs.NArg() == 0 {
		return exitError, errors.New("analyze needs at least one .sql file")
	}
	if *maxRows < 1 {
		return exitError, errors.New("-max-rows must be at least 1")
	}
	opt := analyze.Options{MaxRows: *maxRows}
	if *statsPath != "" {
		st, err := analyze.LoadStats(*statsPath)
		if err != nil {
			return exitError, err
		}
		opt.Stats = st
	}
	type fileResult struct {
		File     string            `json:"file"`
		Findings []analyze.Finding `json:"findings"`
		Worst    analyze.Risk      `json:"worst"`
	}
	var all []fileResult
	worst := analyze.OK
	for _, path := range fs.Args() {
		src, err := readFile(path)
		if err != nil {
			return exitError, err
		}
		fsd, err := analyze.Analyze(string(src), opt)
		if err != nil {
			return exitError, fmt.Errorf("%s: %w", path, err)
		}
		w := analyze.Worst(fsd)
		worst = max(worst, w)
		all = append(all, fileResult{File: path, Findings: fsd, Worst: w})
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(all); err != nil {
			return exitError, err
		}
	} else {
		for _, r := range all {
			printFindings(out, r.File, r.Findings)
		}
		fmt.Fprintf(out, "result: %s\n", worst)
	}
	if worst == analyze.Danger {
		return exitDanger, nil
	}
	return exitOK, nil
}

// printFindings writes the human-readable report for one file.
func printFindings(out io.Writer, file string, fs []analyze.Finding) {
	for _, f := range fs {
		var flags []string
		if f.Rewrite {
			flags = append(flags, "rewrite")
		}
		if f.Scan {
			flags = append(flags, "scan")
		}
		if f.BlocksReads {
			flags = append(flags, "blocks reads")
		}
		if f.BlocksWrites {
			flags = append(flags, "blocks writes")
		}
		fmt.Fprintf(out, "%s:%d %-6s %s", file, f.Line, strings.ToUpper(f.Risk.String()), f.Op)
		if f.Table != "" {
			fmt.Fprintf(out, " on %s", f.Table)
		}
		fmt.Fprintf(out, " | lock %s", f.Lock)
		if len(flags) > 0 {
			fmt.Fprintf(out, " | %s", strings.Join(flags, ", "))
		}
		fmt.Fprintln(out)
		for _, r := range f.Reasons {
			fmt.Fprintf(out, "    - %s\n", r)
		}
		if f.Advice != "" && f.Risk != analyze.OK {
			fmt.Fprintf(out, "    fix: %s\n", f.Advice)
		}
	}
}

// loadSpec reads and validates a spec file.
func loadSpec(fs *flag.FlagSet) (plan.Spec, error) {
	if fs.NArg() != 1 {
		return plan.Spec{}, errors.New("expected exactly one spec.json")
	}
	raw, err := readFile(fs.Arg(0))
	if err != nil {
		return plan.Spec{}, err
	}
	return plan.ParseSpec(raw)
}

// cmdPlan prints the expand/contract SQL and checks it with the analyser.
func cmdPlan(args []string, out io.Writer) error {
	fs := newFlags("plan")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := loadSpec(fs)
	if err != nil {
		return err
	}
	p := plan.Build(spec)
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	script := p.Script()
	fmt.Fprint(out, script)
	findings, err := analyze.Analyze(script, analyze.Options{})
	if err != nil {
		return fmt.Errorf("analyse plan: %w", err)
	}
	fmt.Fprintf(out, "-- lock check of this plan (table size unknown, treated as large): %d statements, worst %s\n",
		len(findings), analyze.Worst(findings))
	return nil
}

// connect opens a connection from $OSM_DATABASE_URL with a connect timeout.
func connect(ctx context.Context, getenv func(string) string) (*pgx.Conn, error) {
	dsn := getenv(envDSN)
	if dsn == "" {
		return nil, fmt.Errorf("%s is not set", envDSN)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envDSN, err)
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}

// cmdRun executes one phase.
func cmdRun(ctx context.Context, args []string, out io.Writer, getenv func(string) string) error {
	fs := newFlags("run")
	phase := fs.String("phase", "", "expand, backfill, switch, contract or rollback")
	retries := fs.Int("retries", 5, "extra attempts after a lock timeout")
	stmtTimeout := fs.Duration("statement-timeout", 5*time.Minute, "timeout for each statement")
	maxBatches := fs.Int("max-batches", 0, "stop the backfill after N batches (0: no limit)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *retries < 0 || *retries > 20 || *stmtTimeout <= 0 || *maxBatches < 0 {
		return errors.New("-retries must be 0-20, -statement-timeout positive, -max-batches not negative")
	}
	spec, err := loadSpec(fs)
	if err != nil {
		return err
	}
	conn, err := connect(ctx, getenv)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	r := &run.Runner{Conn: conn, Plan: plan.Build(spec), Retries: *retries, StatementTimeout: *stmtTimeout,
		MaxBatches: *maxBatches, Log: out}
	res, err := r.Run(ctx, *phase)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "phase %s: skipped=%t steps=%d batches=%d rows=%d transactions=%d in %s\n",
		res.Phase, res.Skipped, res.Steps, res.Batches, res.Rows, res.Txns, res.Duration.Round(time.Millisecond))
	return nil
}

// cmdStatus prints the recorded state of a change.
func cmdStatus(ctx context.Context, args []string, out io.Writer, getenv func(string) string) error {
	fs := newFlags("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := loadSpec(fs)
	if err != nil {
		return err
	}
	conn, err := connect(ctx, getenv)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	st, err := (&run.Runner{Conn: conn, Plan: plan.Build(spec)}).Status(ctx)
	if err != nil {
		return err
	}
	if st.Phase == run.StateNone {
		fmt.Fprintf(out, "%s: not started\n", spec.ID)
		return nil
	}
	fmt.Fprintf(out, "%s: %s (step %d, backfill cursor %d of %d, updated %s)\n",
		spec.ID, st.Phase, st.Step, st.Cursor, st.Target, st.UpdatedAt.Format(time.RFC3339))
	return nil
}
