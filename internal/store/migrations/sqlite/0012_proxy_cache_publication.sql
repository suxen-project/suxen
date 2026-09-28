CREATE TABLE proxy_cache_state (
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    epoch TEXT NOT NULL,
    next_sequence INTEGER NOT NULL,
    published_sequence INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    PRIMARY KEY(repository_id, path)
);
CREATE INDEX proxy_cache_state_expiry ON proxy_cache_state(expires_at_ns);
CREATE INDEX negative_cache_expiry ON negative_cache(expires_at);
