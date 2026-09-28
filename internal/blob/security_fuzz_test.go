package blob

import (
	"strings"
	"testing"
)

func FuzzNormalizeDigest(f *testing.F) {
	f.Add("sha256:" + strings.Repeat("a", 64))
	f.Add("../etc/passwd")
	f.Add("sha512:" + strings.Repeat("0", 128))
	f.Fuzz(func(t *testing.T, value string) {
		normalized, err := NormalizeDigest(value)
		if err == nil && (len(normalized) != len("sha256:")+64 || !strings.HasPrefix(normalized, "sha256:")) {
			t.Fatalf("invalid normalized digest %q", normalized)
		}
	})
}
