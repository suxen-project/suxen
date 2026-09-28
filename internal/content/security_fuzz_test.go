package content

import (
	"encoding/hex"
	"testing"
)

func FuzzBearerChallengeParsing(f *testing.F) {
	for _, seed := range []string{
		`Bearer realm="https://auth.example/token",service="registry"`,
		`Basic realm="registry"`,
		`Bearer realm="https://example.test/a,b",scope="repository:x:pull"`,
		"", "Bearer", "Bearer realm=\"unterminated",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		challenge, ok := parseBearerChallenge(header)
		if ok && challenge.Realm == "" {
			t.Fatal("accepted bearer challenge without a realm")
		}
		if ok {
			prefixed, found := parseBearerChallenge(`Basic realm="registry", ` + header)
			if !found || prefixed != challenge {
				t.Fatalf("Basic prefix changed Bearer challenge: got %+v, found %v; want %+v", prefixed, found, challenge)
			}
			separate, found := parseBearerChallenges([]string{`Basic realm="registry"`, header})
			if !found || separate != challenge {
				t.Fatalf("separate Basic field changed Bearer challenge: got %+v, found %v; want %+v", separate, found, challenge)
			}
		}
	})
}

func FuzzBearerChallengeQuotedScope(f *testing.F) {
	for _, seed := range []string{"", "app", "pull,push", "quoted\\\"value", "\x00\xff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// Hex keeps fuzzed bytes within the quoted-string grammar. A comma and
		// escaped quote still exercise the characters that affect boundaries.
		scope := "repository:" + hex.EncodeToString([]byte(raw)) + `:pull,push"extra`
		header := `Basic realm="other,registry", Bearer realm="https://auth.example/token",scope="` +
			"repository:" + hex.EncodeToString([]byte(raw)) + `:pull,push\"extra"`
		challenge, ok := parseBearerChallenge(header)
		if !ok || challenge.Realm != "https://auth.example/token" || challenge.Scope != scope {
			t.Fatalf("parsed %+v, ok=%v; want scope %q", challenge, ok, scope)
		}
	})
}

func FuzzByteRangeParsing(f *testing.F) {
	for _, seed := range []struct {
		header string
		size   int64
	}{{"bytes=0-0", 1}, {"bytes=-10", 100}, {"bytes=1-", 2}, {"bytes=0-1,2-3", 4}} {
		f.Add(seed.header, seed.size)
	}
	f.Fuzz(func(t *testing.T, header string, size int64) {
		start, end, err := parseSingleByteRange(header, size)
		if err == nil && (size <= 0 || start < 0 || end < start || end >= size) {
			t.Fatalf("accepted invalid range %d-%d for size %d", start, end, size)
		}
	})
}
