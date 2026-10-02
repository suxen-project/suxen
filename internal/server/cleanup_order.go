package server

import (
	"strings"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/versionorder"
)

// retentionRank is what keepLast ordering compares for one asset or unit.
type retentionRank struct {
	// versionKey is the versionorder key, computed once per rank so sorting
	// compares bytes.
	versionKey string
	hasVersion bool
	updatedAt  time.Time
}

func newRetentionRank(version string, hasVersion bool, updatedAt time.Time) retentionRank {
	rank := retentionRank{hasVersion: hasVersion, updatedAt: updatedAt}
	if hasVersion {
		rank.versionKey = versionorder.Key(version)
	}
	return rank
}

// rankedNewer reports whether left is retained ahead of right. Version order
// ranks versioned entries above unversioned ones so the comparison stays a
// strict weak order, and falls back to update time on equal versions.
func rankedNewer(order string, left, right retentionRank) bool {
	if order == domain.CleanupOrderVersion {
		if left.hasVersion != right.hasVersion {
			return left.hasVersion
		}
		if compared := strings.Compare(left.versionKey, right.versionKey); compared != 0 {
			return compared > 0
		}
	}
	return left.updatedAt.After(right.updatedAt)
}
