CREATE TABLE repositories (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    format TEXT NOT NULL,
    type TEXT NOT NULL,
    blob_store TEXT NOT NULL,
    upstream TEXT NOT NULL DEFAULT '',
    members TEXT NOT NULL DEFAULT '[]',
    writable BOOLEAN NOT NULL DEFAULT FALSE,
    format_config TEXT NOT NULL DEFAULT '{}',
    endpoints TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE TABLE assets (
    id BIGSERIAL PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    digest TEXT NOT NULL,
    size BIGINT NOT NULL,
    blob_store TEXT NOT NULL,
    content_type TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'raw',
    reference TEXT NOT NULL DEFAULT '',
    subject_digest TEXT NOT NULL DEFAULT '',
    attributes TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    validated_at TEXT,
    last_accessed TEXT,
    UNIQUE(repository_id, path)
);

CREATE INDEX idx_assets_digest ON assets(repository_id, digest);
CREATE INDEX idx_assets_kind_ref ON assets(repository_id, kind, reference);
CREATE INDEX idx_assets_subject ON assets(repository_id, subject_digest);
CREATE INDEX idx_assets_blob_store_digest ON assets(blob_store, digest);

CREATE TABLE asset_dependencies (
    asset_id BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    digest TEXT NOT NULL,
    PRIMARY KEY(asset_id, digest)
);

CREATE INDEX idx_asset_dependencies_digest ON asset_dependencies(digest);

CREATE TABLE users (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    admin BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TEXT NOT NULL
);

CREATE TABLE tokens (
    id BIGSERIAL PRIMARY KEY,
    username TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    scopes TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL
);

CREATE TABLE roles (
    name TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE role_privileges (
    role TEXT NOT NULL REFERENCES roles(name) ON DELETE CASCADE,
    privilege TEXT NOT NULL,
    PRIMARY KEY(role, privilege)
);

CREATE TABLE role_roles (
    role TEXT NOT NULL REFERENCES roles(name) ON DELETE CASCADE,
    included_role TEXT NOT NULL REFERENCES roles(name) ON DELETE CASCADE,
    PRIMARY KEY(role, included_role),
    CHECK(role <> included_role)
);

CREATE TABLE user_roles (
    username TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    role TEXT NOT NULL REFERENCES roles(name) ON DELETE CASCADE,
    PRIMARY KEY(username, role)
);

CREATE TABLE oidc_providers (
    name TEXT PRIMARY KEY,
    issuer TEXT NOT NULL UNIQUE,
    client_id TEXT NOT NULL,
    client_secret TEXT NOT NULL DEFAULT '',
    scopes TEXT NOT NULL DEFAULT '[]',
    groups_claim TEXT NOT NULL DEFAULT 'groups',
    default_roles TEXT NOT NULL DEFAULT '[]',
    group_roles TEXT NOT NULL DEFAULT '{}',
    allow_password_grant BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TEXT NOT NULL
);

CREATE TABLE negative_cache (
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY(repository_id, path)
);

CREATE TABLE classification_rules (
    repository_id TEXT PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    rules TEXT NOT NULL DEFAULT '[]',
    inherit_global BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TEXT NOT NULL
);

CREATE TABLE classification_defaults (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    rules TEXT NOT NULL DEFAULT '[]',
    managed BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TEXT NOT NULL
);

CREATE TABLE cleanup_policies (
    name TEXT PRIMARY KEY,
    repositories TEXT NOT NULL,
    criteria TEXT NOT NULL,
    keep_last INTEGER NOT NULL DEFAULT 0,
    action TEXT NOT NULL DEFAULT 'delete',
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE tasks (
    id BIGSERIAL PRIMARY KEY,
    type TEXT NOT NULL,
    status TEXT NOT NULL,
    policy TEXT NOT NULL DEFAULT '',
    repository TEXT NOT NULL DEFAULT '',
    dry_run BOOLEAN NOT NULL DEFAULT FALSE,
    result TEXT NOT NULL DEFAULT '{}',
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    started_at TEXT,
    completed_at TEXT
);

CREATE TABLE leader_leases (
    name TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE TABLE webhooks (
    name TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    secret TEXT NOT NULL,
    events TEXT NOT NULL,
    repositories TEXT NOT NULL DEFAULT '[]',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE webhook_deliveries (
    id BIGSERIAL PRIMARY KEY,
    webhook_name TEXT NOT NULL REFERENCES webhooks(name) ON DELETE CASCADE,
    event TEXT NOT NULL,
    repository TEXT NOT NULL,
    payload TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    locked_by TEXT,
    locked_until TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    delivered_at TEXT
);

CREATE INDEX idx_webhook_deliveries_due
    ON webhook_deliveries(status, next_attempt_at, locked_until);

CREATE TABLE download_gates (
    repository_id TEXT PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    criteria TEXT NOT NULL DEFAULT '[]',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    inherit_global BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TEXT NOT NULL
);

CREATE TABLE download_gate_defaults (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    criteria TEXT NOT NULL DEFAULT '[]',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    managed BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TEXT NOT NULL
);

CREATE TABLE trust_policies (
    repository_id TEXT PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    policy TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE trust_policy_defaults (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    policy TEXT NOT NULL,
    managed BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TEXT NOT NULL
);

CREATE TABLE blob_stores (
    name TEXT PRIMARY KEY,
    driver TEXT NOT NULL,
    configuration_env TEXT NOT NULL DEFAULT '',
    configuration_file TEXT NOT NULL DEFAULT '',
    physical_identity TEXT NOT NULL DEFAULT '',
    attributes TEXT NOT NULL DEFAULT '{}',
    state TEXT NOT NULL DEFAULT 'active',
    drain_target TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_blob_stores_physical_identity
ON blob_stores(physical_identity)
WHERE physical_identity <> '';

ALTER TABLE repositories
ADD CONSTRAINT repositories_blob_store_foreign_key
FOREIGN KEY (blob_store) REFERENCES blob_stores(name) ON DELETE RESTRICT;

CREATE TABLE provision_records (
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    spec_hash TEXT NOT NULL,
    secret_fingerprint TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (kind, name)
);

CREATE TABLE upload_sessions (
    id TEXT PRIMARY KEY,
    storage_key TEXT NOT NULL UNIQUE,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
    image TEXT NOT NULL,
    blob_store TEXT NOT NULL,
    principal TEXT NOT NULL,
    size_bytes BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    reserved_bytes BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    operation_id TEXT NOT NULL DEFAULT '',
    operation_expires_at BIGINT NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX idx_upload_sessions_store_usage
ON upload_sessions(blob_store);

CREATE INDEX idx_upload_sessions_principal_usage
ON upload_sessions(blob_store, principal);

CREATE FUNCTION suxen_guard_repository_blob_store_update()
RETURNS trigger AS $$
BEGIN
    IF OLD.blob_store <> NEW.blob_store
        AND EXISTS (SELECT 1 FROM assets WHERE repository_id = OLD.id)
        AND NOT EXISTS (
            SELECT 1 FROM blob_stores
            WHERE name = OLD.blob_store
                AND state = 'draining'
                AND drain_target = NEW.blob_store
        )
    THEN
        RAISE EXCEPTION 'repository blob store in use';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER repositories_blob_store_nonempty_update
BEFORE UPDATE OF blob_store ON repositories
FOR EACH ROW EXECUTE FUNCTION suxen_guard_repository_blob_store_update();

CREATE FUNCTION suxen_guard_repository_immutable_fields()
RETURNS trigger AS $$
BEGIN
    IF OLD.format <> NEW.format OR OLD.type <> NEW.type THEN
        RAISE EXCEPTION 'repository type and format are immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER repositories_immutable_type_format
BEFORE UPDATE OF format, type ON repositories
FOR EACH ROW EXECUTE FUNCTION suxen_guard_repository_immutable_fields();

CREATE FUNCTION suxen_guard_blob_store_change()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.name = 'default' THEN
            RAISE EXCEPTION 'default blob store immutable';
        END IF;
        RETURN OLD;
    END IF;
    IF (OLD.driver <> NEW.driver
        OR OLD.configuration_env <> NEW.configuration_env
        OR OLD.configuration_file <> NEW.configuration_file
        OR OLD.physical_identity <> NEW.physical_identity)
        AND EXISTS (SELECT 1 FROM repositories WHERE blob_store = OLD.name)
    THEN
        RAISE EXCEPTION 'blob store configuration in use';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER blob_stores_configuration_in_use_update
BEFORE UPDATE OF driver, configuration_env, configuration_file, physical_identity ON blob_stores
FOR EACH ROW EXECUTE FUNCTION suxen_guard_blob_store_change();

CREATE TRIGGER blob_stores_default_delete
BEFORE DELETE ON blob_stores
FOR EACH ROW EXECUTE FUNCTION suxen_guard_blob_store_change();
