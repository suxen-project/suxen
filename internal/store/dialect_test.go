package store

import "testing"

// forEachDialect runs body against a migrated SQLite store and, when
// SUXEN_TEST_POSTGRES is configured, a migrated PostgreSQL store, so a
// cross-dialect test states its assertions once. The PostgreSQL sub-test skips
// when the service is not configured; both stores are seeded identically by
// openCompanionTestStore.
func forEachDialect(t *testing.T, body func(t *testing.T, metadata *SQLStore, backend string)) {
	t.Helper()
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			body(t, openCompanionTestStore(t, backend), backend)
		})
	}
}
