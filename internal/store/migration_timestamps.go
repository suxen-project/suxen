package store

import (
	"context"
	"fmt"
	"time"
)

// This inventory contains every timestamp TEXT column in the schema through
// migration 12. Numeric expiry columns are intentionally excluded. Keep the
// inventory complete so all SQL ordering and equality predicates see one
// representation after an upgrade.
var timestampColumns = []struct {
	table   string
	columns []string
}{
	{"repositories", []string{"created_at"}},
	{"assets", []string{"created_at", "updated_at", "validated_at", "last_accessed"}},
	{"users", []string{"created_at"}},
	{"tokens", []string{"created_at"}},
	{"roles", []string{"created_at"}},
	{"oidc_providers", []string{"created_at"}},
	{"negative_cache", []string{"expires_at"}},
	{"classification_rules", []string{"updated_at"}},
	{"classification_defaults", []string{"updated_at"}},
	{"cleanup_policies", []string{"created_at", "updated_at"}},
	{"tasks", []string{"created_at", "started_at", "completed_at"}},
	{"leader_leases", []string{"expires_at"}},
	{"webhooks", []string{"created_at", "updated_at"}},
	{"webhook_deliveries", []string{"next_attempt_at", "locked_until", "created_at", "updated_at", "delivered_at"}},
	{"download_gates", []string{"updated_at"}},
	{"download_gate_defaults", []string{"updated_at"}},
	{"trust_policies", []string{"updated_at"}},
	{"trust_policy_defaults", []string{"updated_at"}},
	{"blob_stores", []string{"created_at"}},
	{"provision_records", []string{"updated_at"}},
	{"upload_sessions", []string{"created_at", "updated_at"}},
	{"schema_migrations", []string{"applied_at"}},
}

func validateLegacyTimestamps(ctx context.Context, tx *dialectTx) error {
	for _, entry := range timestampColumns {
		for _, column := range entry.columns {
			// Validate before transforming. Historical writes used the canonical
			// UTC RFC3339Nano form; an unexpected value must fail the atomic
			// migration rather than be silently corrupted by string slicing.
			query := fmt.Sprintf("SELECT DISTINCT %s FROM %s WHERE %s IS NOT NULL", column, entry.table, column)
			rows, err := tx.QueryContext(ctx, query)
			if err != nil {
				return fmt.Errorf("read %s.%s: %w", entry.table, column, err)
			}
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					rows.Close()
					return fmt.Errorf("scan %s.%s: %w", entry.table, column, err)
				}
				parsed, err := time.Parse(time.RFC3339Nano, value)
				if err != nil || value != parsed.UTC().Format(time.RFC3339Nano) && value != formatTime(parsed) {
					rows.Close()
					return fmt.Errorf("unexpected timestamp %q in %s.%s", value, entry.table, column)
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s.%s: %w", entry.table, column, err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("close %s.%s: %w", entry.table, column, err)
			}
		}
	}
	return nil
}
