ALTER TABLE users ADD COLUMN identity TEXT NOT NULL DEFAULT '';
UPDATE users SET identity = lower(hex(randomblob(18)));
CREATE UNIQUE INDEX idx_users_identity ON users(identity);
