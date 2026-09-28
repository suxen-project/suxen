-- Normalize legacy UTC RFC3339Nano strings to fixed nine-digit fractional seconds.
-- Existing NULL values remain NULL; the migration is applied in one transaction.
UPDATE repositories SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE assets SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE assets SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE assets SET validated_at = substr(validated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(validated_at) > 20 THEN substr(validated_at, 21, length(validated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE validated_at IS NOT NULL;
UPDATE assets SET last_accessed = substr(last_accessed, 1, 19) || '.' ||
    substr((CASE WHEN length(last_accessed) > 20 THEN substr(last_accessed, 21, length(last_accessed) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE last_accessed IS NOT NULL;
UPDATE users SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE tokens SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE roles SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE oidc_providers SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE negative_cache SET expires_at = substr(expires_at, 1, 19) || '.' ||
    substr((CASE WHEN length(expires_at) > 20 THEN substr(expires_at, 21, length(expires_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE expires_at IS NOT NULL;
UPDATE classification_rules SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE classification_defaults SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE cleanup_policies SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE cleanup_policies SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE tasks SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE tasks SET started_at = substr(started_at, 1, 19) || '.' ||
    substr((CASE WHEN length(started_at) > 20 THEN substr(started_at, 21, length(started_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE started_at IS NOT NULL;
UPDATE tasks SET completed_at = substr(completed_at, 1, 19) || '.' ||
    substr((CASE WHEN length(completed_at) > 20 THEN substr(completed_at, 21, length(completed_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE completed_at IS NOT NULL;
UPDATE leader_leases SET expires_at = substr(expires_at, 1, 19) || '.' ||
    substr((CASE WHEN length(expires_at) > 20 THEN substr(expires_at, 21, length(expires_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE expires_at IS NOT NULL;
UPDATE webhooks SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE webhooks SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE webhook_deliveries SET next_attempt_at = substr(next_attempt_at, 1, 19) || '.' ||
    substr((CASE WHEN length(next_attempt_at) > 20 THEN substr(next_attempt_at, 21, length(next_attempt_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE next_attempt_at IS NOT NULL;
UPDATE webhook_deliveries SET locked_until = substr(locked_until, 1, 19) || '.' ||
    substr((CASE WHEN length(locked_until) > 20 THEN substr(locked_until, 21, length(locked_until) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE locked_until IS NOT NULL;
UPDATE webhook_deliveries SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE webhook_deliveries SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE webhook_deliveries SET delivered_at = substr(delivered_at, 1, 19) || '.' ||
    substr((CASE WHEN length(delivered_at) > 20 THEN substr(delivered_at, 21, length(delivered_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE delivered_at IS NOT NULL;
UPDATE download_gates SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE download_gate_defaults SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE trust_policies SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE trust_policy_defaults SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE blob_stores SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE provision_records SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE upload_sessions SET created_at = substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN length(created_at) > 20 THEN substr(created_at, 21, length(created_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE created_at IS NOT NULL;
UPDATE upload_sessions SET updated_at = substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN length(updated_at) > 20 THEN substr(updated_at, 21, length(updated_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE updated_at IS NOT NULL;
UPDATE schema_migrations SET applied_at = substr(applied_at, 1, 19) || '.' ||
    substr((CASE WHEN length(applied_at) > 20 THEN substr(applied_at, 21, length(applied_at) - 21)
        ELSE '' END) || '000000000', 1, 9) || 'Z'
    WHERE applied_at IS NOT NULL;
