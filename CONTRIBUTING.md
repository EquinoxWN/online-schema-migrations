# Contributing

Thanks for helping. This is a Go project; issues and pull requests are welcome.

## Set up and check your change

Needs Go 1.25 or newer (CI uses 1.26.11). Caches stay in git-ignored folders inside the repo.

```bash
make setup      # download modules
make lint       # gofmt and go vet
make test       # unit tests; PostgreSQL tests skip without a server
make pg-start   # local PostgreSQL 17 from embedded binaries in .tmp/ (no Docker)
export OSM_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:54329/postgres?sslmode=disable'
make test-pg    # every test, against that server; fails if any test skips
make pg-stop
make audit      # known-vulnerability check (as in CI)
```

On Windows without `make`, run the commands from the `Makefile` directly. The race detector
needs cgo; CI runs it.

A pull request is ready when `make lint` and `make test-pg` pass and CI is green.

## Guidelines

- A new lock rule needs a row in `TestStatementRules` and, if the statement can run inside a
  transaction, a row in `TestPGLockPredictionsMatchServer`, so the prediction is checked against
  a real server.
- Keep each change focused; explain the problem it solves in the pull request.
- Add or update a test for every behaviour change, and update `docs/results/` when numbers change.
- Significant design changes need an ADR in `docs/adr/`.
- Keep function comments to one short phrase.
- Report security problems privately, as described in [SECURITY.md](SECURITY.md).

By contributing you agree that your work is released under the [MIT License](LICENSE).
