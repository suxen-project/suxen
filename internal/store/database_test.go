package store

import "testing"

func TestBindPostgresParameters(t *testing.T) {
	query := `SELECT '?' AS literal, value FROM records WHERE first = ? AND second = ?`
	expected := `SELECT '?' AS literal, value FROM records WHERE first = $1 AND second = $2`
	if actual := bindPostgres(query); actual != expected {
		t.Fatalf("bindPostgres() = %q, want %q", actual, expected)
	}
}

func TestBindPostgresEscapedQuotes(t *testing.T) {
	query := `SELECT 'it''s ?' FROM records WHERE value = ?`
	expected := `SELECT 'it''s ?' FROM records WHERE value = $1`
	if actual := bindPostgres(query); actual != expected {
		t.Fatalf("bindPostgres() = %q, want %q", actual, expected)
	}
}
