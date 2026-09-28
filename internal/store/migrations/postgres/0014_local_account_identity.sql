ALTER TABLE users ADD COLUMN identity TEXT;
UPDATE users SET identity = gen_random_uuid()::text;
ALTER TABLE users ALTER COLUMN identity SET NOT NULL;
CREATE UNIQUE INDEX idx_users_identity ON users(identity);
