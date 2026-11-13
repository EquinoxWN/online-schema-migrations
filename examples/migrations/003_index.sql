-- CREATE INDEX CONCURRENTLY must run outside a transaction block.
CREATE INDEX CONCURRENTLY IF NOT EXISTS users_email_idx ON users (email);
