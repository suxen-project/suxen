package identity

import "testing"

func TestSafeRedirectPathRejectsBrowserAmbiguities(t *testing.T) {
	for _, candidate := range []string{
		`/\attacker.example/path`,
		`/%5cattacker.example/path`,
		`/%5Cattacker.example/path`,
		`/%255cattacker.example/path`,
		`/%2f%2fattacker.example/path`,
		"/safe\nLocation: https://attacker.example",
		"//attacker.example/path",
		"https://attacker.example/path",
	} {
		t.Run(candidate, func(t *testing.T) {
			if got := safeRedirectPath(candidate); got != "/" {
				t.Fatalf("safeRedirectPath(%q) = %q, want /", candidate, got)
			}
		})
	}

	for _, candidate := range []string{
		"/",
		"/repository/raw/package.tar.gz",
		"/ui/#/repositories?name=release",
		"/path/with%20space?tab=assets",
	} {
		t.Run("local "+candidate, func(t *testing.T) {
			if got := safeRedirectPath(candidate); got != candidate {
				t.Fatalf("safeRedirectPath(%q) = %q", candidate, got)
			}
		})
	}
}
