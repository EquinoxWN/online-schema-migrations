.PHONY: setup lint test test-pg pg-start pg-stop fuzz demo ci audit

setup:
	go mod download

lint:
	@files="$$(gofmt -l cmd internal)"; test -z "$$files" || (echo "$$files"; echo "run gofmt -w cmd internal"; exit 1)
	go vet ./...

# Unit tests; tests that need PostgreSQL skip unless OSM_TEST_DATABASE_URL is set.
test:
	go test -race -count=1 ./...

# All tests against a running PostgreSQL (CI service container, or `make pg-start` locally); fails on any skip.
test-pg:
	@test -n "$$OSM_TEST_DATABASE_URL" || (echo "set OSM_TEST_DATABASE_URL"; exit 1)
	go test -race -count=1 -v ./... > test-pg.log 2>&1 || (tail -50 test-pg.log; exit 1)
	@! grep -q -- '--- SKIP' test-pg.log
	@tail -6 test-pg.log

# Local PostgreSQL 17 from embedded binaries in .tmp/ (no Docker).
pg-start:
	bash scripts/local-postgres.sh start

pg-stop:
	bash scripts/local-postgres.sh stop

fuzz:
	go test -run '^$$' -fuzz FuzzSplit -fuzztime 30s ./internal/sqlscan

demo:
	-go run ./cmd/osm analyze -stats examples/stats.json examples/migrations/001_risky.sql
	go run ./cmd/osm analyze -stats examples/stats.json examples/migrations/002_safe.sql examples/migrations/003_index.sql
	go run ./cmd/osm plan examples/email_ci.json

# Known vulnerabilities in the code paths actually called (needs Go 1.26+, as in CI).
audit:
	go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

ci: setup lint test
