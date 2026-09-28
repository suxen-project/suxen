package predicate

import (
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func FuzzPredicateEvaluation(f *testing.F) {
	for _, seed := range []struct{ path, operator, actual, expected string }{
		{"sys.path", "contains", "release/app.jar", "release"},
		{"value", "matches", "abc", "^a"},
		{"missing", "absent", "", ""},
	} {
		f.Add(seed.path, seed.operator, seed.actual, seed.expected)
	}
	f.Fuzz(func(t *testing.T, path, operator, actual, expected string) {
		attributes := map[string]any{"value": actual, "sys": map[string]any{"path": actual}}
		_ = Match(attributes, domain.Predicate{Path: path, Op: operator, Value: expected}, time.Unix(0, 0))
	})
}
