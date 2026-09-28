package npm

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestResolveProxyRequestSelectsStrongestAdvertisedIntegrity(t *testing.T) {
	strongA := sha512.Sum512([]byte("first acceptable archive"))
	strongB := sha512.Sum512([]byte("second acceptable archive"))
	weak := sha1.Sum([]byte("incorrect archive"))
	integrity := fmt.Sprintf("sha1-%s sha512-%s sha999-ignored sha512-%s",
		base64.StdEncoding.EncodeToString(weak[:]),
		base64.StdEncoding.EncodeToString(strongA[:]),
		base64.StdEncoding.EncodeToString(strongB[:]))
	stored := npmProxyPackument(fmt.Sprintf(`{"versions":{"1.0.0":{"dist":{"tarball":"https://tar.example/pkg.tgz","integrity":%q,"shasum":"%s"}}}}`, integrity, hex.EncodeToString(weak[:])))
	resolved, err := (Format{}).ResolveProxyRequest(context.Background(), format.Repository{}, "pkg/-/pkg-1.0.0.tgz", "", stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.ExpectedDigests) != 2 ||
		!slices.Contains(resolved.ExpectedDigests, "sha512:"+hex.EncodeToString(strongA[:])) ||
		!slices.Contains(resolved.ExpectedDigests, "sha512:"+hex.EncodeToString(strongB[:])) {
		t.Fatalf("expected strongest SHA-512 alternatives, got %v", resolved.ExpectedDigests)
	}
	if resolved.CachePath == "pkg/-/pkg-1.0.0.tgz" || resolved.UpstreamURL != "https://tar.example/pkg.tgz" {
		t.Fatalf("resolved request = %+v", resolved)
	}
	changed := npmProxyPackument(strings.ReplaceAll(string(stored), "https://tar.example/pkg.tgz", "https://other.example/pkg.tgz"))
	second, err := (Format{}).ResolveProxyRequest(context.Background(), format.Repository{}, "pkg/-/pkg-1.0.0.tgz", "", changed)
	if err != nil || second.CachePath == resolved.CachePath {
		t.Fatalf("changed upstream target reused cache identity: %q, %q; error %v", resolved.CachePath, second.CachePath, err)
	}
	strongC := sha512.Sum512([]byte("changed advertised archive"))
	changedDigest := npmProxyPackument(strings.ReplaceAll(string(stored),
		base64.StdEncoding.EncodeToString(strongA[:]), base64.StdEncoding.EncodeToString(strongC[:])))
	third, err := (Format{}).ResolveProxyRequest(context.Background(), format.Repository{}, "pkg/-/pkg-1.0.0.tgz", "", changedDigest)
	if err != nil || third.CachePath == resolved.CachePath {
		t.Fatalf("changed advertised digest reused cache identity: %q, %q; error %v", resolved.CachePath, third.CachePath, err)
	}
	reordered := npmProxyPackument(strings.ReplaceAll(string(stored), integrity,
		fmt.Sprintf("sha512-%s sha999-ignored sha512-%s sha1-%s",
			base64.StdEncoding.EncodeToString(strongB[:]),
			base64.StdEncoding.EncodeToString(strongA[:]),
			base64.StdEncoding.EncodeToString(weak[:]))))
	fourth, err := (Format{}).ResolveProxyRequest(context.Background(), format.Repository{}, "pkg/-/pkg-1.0.0.tgz", "", reordered)
	if err != nil || fourth.CachePath != resolved.CachePath {
		t.Fatalf("equivalent integrity token order changed cache identity: %q, %q; error %v", resolved.CachePath, fourth.CachePath, err)
	}
}

func TestAdvertisedDigestsUsesLegacyShasumOnlyWithoutIntegrity(t *testing.T) {
	sum := sha1.Sum([]byte("archive"))
	got, err := advertisedDigests(nil, []byte(fmt.Sprintf("%q", hex.EncodeToString(sum[:]))))
	if err != nil || len(got) != 1 || got[0] != "sha1:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy shasum = %v, %v", got, err)
	}
	if _, err := advertisedDigests([]byte(`"sha512-invalid"`), []byte(fmt.Sprintf("%q", hex.EncodeToString(sum[:])))); err == nil {
		t.Fatal("malformed strongest SRI token fell back to shasum")
	}
	if _, err := advertisedDigests([]byte(`"sha999-ignored"`), nil); err == nil {
		t.Fatal("integrity with no supported algorithm was accepted")
	}
	sha256Digest := sha256.Sum256([]byte("archive"))
	got, err = advertisedDigests([]byte(fmt.Sprintf("%q", "sha256-"+base64.StdEncoding.EncodeToString(sha256Digest[:]))), nil)
	if err != nil || len(got) != 1 || got[0] != "sha256:"+hex.EncodeToString(sha256Digest[:]) {
		t.Fatalf("SHA-256 integrity = %v, %v", got, err)
	}
}

func TestResolveProxyRequestRecoversOnlyUnambiguousWarmTarball(t *testing.T) {
	const path = "pkg/-/pkg-1.0.0.tgz"
	const prefix = path + proxyIdentityMarker
	first := prefix + strings.Repeat("a", 64)
	second := prefix + strings.Repeat("b", 64)
	ctx := context.Background()
	resolve := func(paths ...string) format.ResolvedProxyRequest {
		t.Helper()
		result, err := (Format{}).ResolveProxyRequest(ctx, format.Repository{}, path, "", npmCachedPaths(paths))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := resolve(first); got.CachePath != first || !got.CacheOnly {
		t.Fatalf("one warm identity resolved to %+v", got)
	}
	if got := resolve(first, second); got.CachePath != path || got.CacheOnly {
		t.Fatalf("ambiguous historical identities resolved to %+v", got)
	}
	if got := resolve(prefix+strings.Repeat("g", 64), first+"-other"); got.CachePath != path {
		t.Fatalf("malformed identity resolved to %q", got.CachePath)
	}
	if got := resolve(path + "-other" + proxyIdentityMarker + strings.Repeat("a", 64)); got.CachePath != path {
		t.Fatalf("another public path resolved to %q", got.CachePath)
	}
}

func TestResolveProxyRequestRemovedVersionUsesOnlyUnambiguousCache(t *testing.T) {
	const path = "pkg/-/pkg-1.0.0.tgz"
	const prefix = path + proxyIdentityMarker
	first := prefix + strings.Repeat("a", 64)
	second := prefix + strings.Repeat("b", 64)
	for _, test := range []struct {
		name     string
		paths    []string
		wantPath string
	}{
		{"one identity", []string{first}, first},
		{"ambiguous identities", []string{first, second}, path},
		{"no identity", nil, path},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := npmCachedPackument{npmCachedPaths: test.paths, content: []byte(`{"versions":{}}`)}
			got, err := (Format{}).ResolveProxyRequest(context.Background(), format.Repository{}, path, "", stored)
			if err != nil || got.CachePath != test.wantPath || !got.CacheOnly || got.UpstreamURL != "" {
				t.Fatalf("removed version resolved to %+v, %v", got, err)
			}
		})
	}
}

type npmCachedPackument struct {
	npmCachedPaths
	content []byte
}

func (stored npmCachedPackument) ReadAsset(_ context.Context, _ string) ([]byte, bool, error) {
	return stored.content, true, nil
}

type npmCachedPaths []string

func (paths npmCachedPaths) ReadAsset(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, nil
}

func (paths npmCachedPaths) VisitAssetPaths(_ context.Context, prefix string, visit func(string) (bool, error)) error {
	for _, path := range paths {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		cont, err := visit(path)
		if err != nil || !cont {
			return err
		}
	}
	return nil
}

type npmProxyPackument string

func (stored npmProxyPackument) ReadAsset(_ context.Context, _ string) ([]byte, bool, error) {
	return []byte(stored), true, nil
}

func (stored npmProxyPackument) VisitAssetPaths(_ context.Context, _ string, _ func(string) (bool, error)) error {
	return nil
}
