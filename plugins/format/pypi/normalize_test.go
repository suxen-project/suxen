package pypi

import (
	"regexp"
	"strings"
	"testing"
)

func TestNormalizeProjectNamePreservesPEP503Behavior(t *testing.T) {
	pattern := regexp.MustCompile(`[-_.]+`)
	for _, name := range []string{
		"", "Simple", "_Leading", "trailing.-_", "MIXED._-Separators",
		"already-normalized", "A..B__C--D", "Ünicode._Ä", "a_雪.B",
		string([]byte{'A', '_', 0xff, '.', 'B'}),
	} {
		want := strings.ToLower(pattern.ReplaceAllString(name, "-"))
		if got := normalizeProjectName(name); got != want {
			t.Errorf("normalizeProjectName(%q) = %q, want %q", name, got, want)
		}
	}
}

func BenchmarkNormalizeProjectName(b *testing.B) {
	name := "package-with-a-realistically-long-project-name-000000"
	for b.Loop() {
		_ = normalizeProjectName(name)
	}
}
