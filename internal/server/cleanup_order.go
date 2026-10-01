package server

import (
	"cmp"
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"golang.org/x/mod/semver"
)

// retentionRank is what keepLast ordering compares for one asset or unit.
type retentionRank struct {
	version    string
	hasVersion bool
	updatedAt  time.Time
}

// rankedNewer reports whether left is retained ahead of right. Version order
// ranks versioned entries above unversioned ones so the comparison stays a
// strict weak order, and falls back to update time on equal versions.
func rankedNewer(order string, left, right retentionRank) bool {
	if order == domain.CleanupOrderVersion {
		if left.hasVersion != right.hasVersion {
			return left.hasVersion
		}
		if left.hasVersion {
			if compared := compareVersions(left.version, right.version); compared != 0 {
				return compared > 0
			}
		}
	}
	return left.updatedAt.After(right.updatedAt)
}

// compareVersions uses semantic version precedence when both values parse as
// semver, with or without a leading "v", and natural string order otherwise.
func compareVersions(left, right string) int {
	leftSemver, rightSemver := semverForm(left), semverForm(right)
	if semver.IsValid(leftSemver) && semver.IsValid(rightSemver) {
		return semver.Compare(leftSemver, rightSemver)
	}
	return compareNatural(left, right)
}

func semverForm(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

// compareNatural orders digit runs numerically and other runs bytewise, so
// "build-10" sorts after "build-9".
func compareNatural(left, right string) int {
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

func nextRun(value string) (run, rest string, digits bool) {
	digits = isDigit(value[0])
	end := 1
	for end < len(value) && isDigit(value[end]) == digits {
		end++
	}
	return value[:end], value[end:], digits
}

func isDigit(character byte) bool {
	return character >= '0' && character <= '9'
}
