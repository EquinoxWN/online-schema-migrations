// Package pgtest gives integration tests an isolated schema on a real PostgreSQL server.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvURL names the variable with the test server's connection string.
const EnvURL = "OSM_TEST_DATABASE_URL"

// DB is one isolated schema; every connection from it uses that schema.
type DB struct {
	t      *testing.T
	url    string
	Schema string
}

// New creates a fresh schema, or skips the test when no server is configured.
func New(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skipf("%s not set: integration test needs PostgreSQL (CI service, or scripts/local-postgres.sh start)", EnvURL)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	db := &DB{t: t, url: url, Schema: "osm_test_" + hex.EncodeToString(b)}
	admin := db.connect("public")
	db.Exec(admin, "CREATE SCHEMA "+db.Schema)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := db.connect("public")
		// osm_state is created in the test schema (search_path), so it goes too.
		if _, err := c.Exec(ctx, "DROP SCHEMA "+db.Schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	return db
}

// Conn opens a connection whose search_path is the test schema; it is closed at cleanup.
func (d *DB) Conn() *pgx.Conn { return d.connect(d.Schema) }

// connect opens a connection with the given search_path.
func (d *DB) connect(schema string) *pgx.Conn {
	d.t.Helper()
	cfg, err := pgx.ParseConfig(d.url)
	if err != nil {
		d.t.Fatalf("%s: %v", EnvURL, err)
	}
	cfg.ConnectTimeout = 10 * time.Second
	cfg.RuntimeParams["search_path"] = schema
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		d.t.Fatalf("connect: %v", err)
	}
	d.t.Cleanup(func() { _ = c.Close(context.Background()) }) // test teardown; nothing to recover
	return c
}

// Exec runs SQL and fails the test on error.
func (d *DB) Exec(c *pgx.Conn, sql string, args ...any) {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

// Int runs a query returning one integer.
func (d *DB) Int(c *pgx.Conn, sql string, args ...any) int64 {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var n int64
	if err := c.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	return n
}
