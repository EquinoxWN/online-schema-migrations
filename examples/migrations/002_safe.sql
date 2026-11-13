-- The same intent, written so that no statement holds a blocking lock for long.
SET lock_timeout = '2s';
ALTER TABLE users ADD COLUMN token uuid;
ALTER TABLE users ALTER COLUMN token SET DEFAULT gen_random_uuid();
ALTER TABLE users ALTER COLUMN email TYPE varchar(200);
ALTER TABLE users ADD CONSTRAINT users_email_nn CHECK (email IS NOT NULL) NOT VALID;
ALTER TABLE users VALIDATE CONSTRAINT users_email_nn;
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
