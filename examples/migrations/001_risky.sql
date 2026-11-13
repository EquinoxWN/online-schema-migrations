-- A migration that looks harmless but would block a large users table.
ALTER TABLE users ADD COLUMN token uuid DEFAULT gen_random_uuid();
ALTER TABLE users ALTER COLUMN age TYPE bigint;
CREATE INDEX users_email_idx ON users (email);
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
UPDATE users SET email = lower(email);
