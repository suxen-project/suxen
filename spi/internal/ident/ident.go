// Package ident is shared identifier grammar for SPI registries.
package ident

// Valid reports whether name is a lowercase identifier: a letter followed by
// letters, digits, or hyphens. Job names, plugin IDs, and format names use
// this grammar.
func Valid(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if index > 0 && ((r >= '0' && r <= '9') || r == '-') {
			continue
		}
		return false
	}
	return true
}
