// Package versionorder defines the version ordering that cleanup and the
// component listing share, as a key whose plain byte order is the ordering, so
// comparing keys is cheap and always a total order.
//
// Two versions that both parse as semantic versions (with or without a leading
// "v", including Go's shorthand forms) order by semver precedence; build
// metadata is ignored. Two other versions order naturally, after dropping a
// leading "v" that precedes a digit: digit runs compare numerically and other
// runs bytewise, so "build-10" follows "build-9". A
// semantic version orders before any other version that shares its numeric
// major.minor.patch prefix and continues past it; otherwise the natural order
// of the leading numbers decides, so "2026.9.30" precedes "2026.10.01".
//
// Encoding: a digit run is '0', its significant-digit count as four decimal
// digits, then the digits without leading zeros; any other run is its bytes
// followed by 0x01. A semantic version encodes major.minor.patch that way, then
// 0x03 for a release or 0x02 followed by its prerelease identifiers (numeric:
// '0', count, digits; alphanumeric: 'A', bytes, 0x01). Keys longer than
// MaxKeyLength bytes are truncated at a UTF-8 boundary, so versions that only
// differ beyond that point compare equal.
package versionorder

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/mod/semver"
)

// MaxKeyLength bounds a stored key so it fits comfortably in an index entry.
const MaxKeyLength = 512

// Key returns the sortable key of a version.
func Key(version string) string {
	var key strings.Builder
	if canonical := semverCanonical(version); canonical != "" {
		release, prerelease, hasPrerelease := strings.Cut(strings.TrimPrefix(canonical, "v"), "-")
		writeNatural(&key, release)
		if hasPrerelease {
			key.WriteByte(0x02)
			for _, identifier := range strings.Split(prerelease, ".") {
				if isDigits(identifier) {
					writeNumber(&key, identifier)
				} else {
					key.WriteByte('A')
					key.WriteString(identifier)
					key.WriteByte(0x01)
				}
			}
		} else {
			key.WriteByte(0x03)
		}
	} else {
		writeNatural(&key, trimVersionPrefix(version))
	}
	// PostgreSQL text must be valid UTF-8; Raw paths and OCI tags already are.
	return truncate(strings.ToValidUTF8(key.String(), "\uFFFD"))
}

// Compare orders two versions by their keys.
func Compare(left, right string) int {
	return strings.Compare(Key(left), Key(right))
}

func semverCanonical(version string) string {
	form := version
	if !strings.HasPrefix(form, "v") {
		form = "v" + form
	}
	if !semver.IsValid(form) {
		return ""
	}
	// Canonical drops build metadata, which semver precedence ignores.
	return semver.Canonical(form)
}

// trimVersionPrefix drops a leading "v" before a digit, as semver parsing
// does, so "v1.2.3.4" orders among the numbered versions rather than after
// them.
func trimVersionPrefix(version string) string {
	if len(version) > 1 && version[0] == 'v' && isDigit(version[1]) {
		return version[1:]
	}
	return version
}

func writeNatural(key *strings.Builder, value string) {
	for value != "" {
		end := 1
		digits := isDigit(value[0])
		for end < len(value) && isDigit(value[end]) == digits {
			end++
		}
		if digits {
			writeNumber(key, value[:end])
		} else {
			key.WriteString(value[:end])
			key.WriteByte(0x01)
		}
		value = value[end:]
	}
}

func writeNumber(key *strings.Builder, digits string) {
	significant := strings.TrimLeft(digits, "0")
	key.WriteByte('0')
	fmt.Fprintf(key, "%04d", len(significant))
	key.WriteString(significant)
}

func truncate(key string) string {
	if len(key) <= MaxKeyLength {
		return key
	}
	key = key[:MaxKeyLength]
	for !utf8.ValidString(key) {
		key = key[:len(key)-1]
	}
	return key
}

func isDigits(value string) bool {
	for index := 0; index < len(value); index++ {
		if !isDigit(value[index]) {
			return false
		}
	}
	return value != ""
}

func isDigit(character byte) bool {
	return character >= '0' && character <= '9'
}
