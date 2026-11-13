# Security policy

## Reporting a vulnerability

Please report security problems privately through GitHub:
**Security > Report a vulnerability** on this repository
(`https://github.com/EquinoxWN/online-schema-migrations/security/advisories/new`). Do not open a public issue.

Include what you found, how to reproduce it, and the impact you expect. You will get an answer
within 7 days. Fixes are released as soon as they are ready, and you are credited unless you ask
not to be.

## Supported versions

This project is pre-1.0. Only the latest commit on `main` receives fixes.

## Scope

- **In scope:** SQL injection through a spec file's names or types, a plan that loses or corrupts
  data, a lock prediction that calls a blocking statement safe, and crashes on malformed SQL input.
- **Out of scope:** The `up` and `down` expressions of a spec are SQL written by the migration
  author and run as given, like any migration file; performance problems; roadmap features.

## How this repository protects itself

- Table, column and key names in a spec must match `^[a-z_][a-z0-9_]{0,62}$` and are always
  double-quoted; types must be plain type names; expressions may not contain `;` or `$osm$` and
  must have balanced parentheses. Unknown JSON fields are rejected.
- The connection string comes from `OSM_DATABASE_URL`, never from a command-line argument; put
  passwords in `~/.pgpass` or `PGPASSWORD`, not in the URL.
- Every GitHub Action is pinned to a full commit SHA and the PostgreSQL service image to a digest;
  workflows run with read-only permissions and without persisted credentials.
- Dependabot update pull requests are paused (`open-pull-requests-limit: 0`) so the repository
  keeps a single `main` branch; dependencies are updated by hand and checked by the CI audit.
- CI runs lint, tests (with the race detector) and `govulncheck` on every push and pull request.
