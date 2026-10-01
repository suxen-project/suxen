package versionorder

import (
	"cmp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
)

// naturalCompare is the reference numeric-aware order for non-semver pairs.
func naturalCompare(left, right string) int {
	for left != "" && right != "" {
		leftRun, leftRest, leftDigits := nextRun(left)
		rightRun, rightRest, rightDigits := nextRun(right)
		if leftDigits && rightDigits {
			leftNumber := strings.TrimLeft(leftRun, "0")
			rightNumber := strings.TrimLeft(rightRun, "0")
			if len(leftNumber) != len(rightNumber) {
				return cmp.Compare(len(leftNumber), len(rightNumber))
			}
			if compared := strings.Compare(leftNumber, rightNumber); compared != 0 {
				return compared
			}
		} else if compared := strings.Compare(leftRun, rightRun); compared != 0 {
			return compared
		}
		left, right = leftRest, rightRest
	}
	return cmp.Compare(len(left), len(right))
}

func nextRun(value string) (string, string, bool) {
	digits := isDigit(value[0])
	end := 1
	for end < len(value) && isDigit(value[end]) == digits {
		end++
	}
	return value[:end], value[end:], digits
}

func isSemver(version string) bool {
	return semverCanonical(version) != ""
}

var versions = []string{
	"0.9.0", "0.10.0", "0.9", "0.10", "1", "v1", "1.0.0", "v1.0.0", "1.0.0+build.5",
	"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
	"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0-0.3.7", "1.0.0-x.7.z.92", "2.0.0", "v2.1.3",
	"2026.9.30", "2026.10.01", "2026.10.1-1", "build-9", "build-10", "build-010",
	"alpha", "beta", "alpha.11152", "alpha.11153", "r10", "r9", "", "v", "vNext",
	"1.0.0.1", "1.0.0x", "1.0.0-", "release_2", "release-2", "ü-1", "ü-10",
}

func sign(value int) int {
	return cmp.Compare(value, 0)
}

func TestKeyOrderMatchesSemverAndNaturalOrderWithinEachKind(t *testing.T) {
	for _, left := range versions {
		for _, right := range versions {
			got := sign(Compare(left, right))
			leftSemver, rightSemver := isSemver(left), isSemver(right)
			switch {
			case leftSemver && rightSemver:
				if want := semver.Compare(semverCanonical(left), semverCanonical(right)); got != want {
					t.Errorf("Compare(%q, %q) = %d, semver precedence = %d", left, right, got, want)
				}
			case !leftSemver && !rightSemver:
				if want := sign(naturalCompare(left, right)); got != want {
					t.Errorf("Compare(%q, %q) = %d, natural order = %d", left, right, got, want)
				}
			}
		}
	}
}

func TestKeyOrderIsATotalOrderAcrossKinds(t *testing.T) {
	sorted := slices.Clone(versions)
	slices.SortStableFunc(sorted, Compare)
	for index := 1; index < len(sorted); index++ {
		if Compare(sorted[index-1], sorted[index]) > 0 {
			t.Fatalf("sort produced %q before %q", sorted[index-1], sorted[index])
		}
	}
	for _, a := range versions {
		for _, b := range versions {
			for _, c := range versions {
				if Compare(a, b) <= 0 && Compare(b, c) <= 0 && Compare(a, c) > 0 {
					t.Fatalf("order is not transitive: %q <= %q <= %q but %q > %q", a, b, c, a, c)
				}
			}
		}
	}
	for _, tc := range []struct {
		lower, higher string
	}{
		{"0.9.0", "0.10.0"},
		{"1.0.0-rc.1", "1.0.0"},
		{"1.0.0", "1.0.0.1"},
		{"2026.9.30", "2026.10.01"},
		{"build-9", "build-10"},
		{"alpha.11152", "alpha.11153"},
	} {
		if Compare(tc.lower, tc.higher) >= 0 {
			t.Errorf("Compare(%q, %q) >= 0", tc.lower, tc.higher)
		}
	}
	if Compare("v1.2.3", "1.2.3+meta") != 0 {
		t.Error("equal semantic versions must have equal keys")
	}
}

func TestKeyIsBoundedValidUTF8(t *testing.T) {
	long := strings.Repeat("ü9", MaxKeyLength)
	key := Key(long)
	if len(key) > MaxKeyLength || !utf8Valid(key) {
		t.Fatalf("key length %d, valid=%t", len(key), utf8Valid(key))
	}
	if Key("\xff1") == "" || !utf8Valid(Key("\xff1")) {
		t.Fatal("invalid UTF-8 input must yield a valid key")
	}
}

func utf8Valid(value string) bool {
	return strings.ToValidUTF8(value, "") == value
}
