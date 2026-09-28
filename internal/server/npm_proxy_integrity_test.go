package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	_ "github.com/suxen-project/suxen/plugins/format/npm"
)

func proxyNpmArchive(t *testing.T, version string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	content := []byte(fmt.Sprintf(`{"name":"pkg","version":%q}`, version))
	if err := archive.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestNpmProxyRejectsTarballThatViolatesAdvertisedIntegrity(t *testing.T) {
	for _, scheme := range []string{"multi-token SRI", "legacy shasum"} {
		t.Run(scheme, func(t *testing.T) {
			fixture := newServerFixture(t)
			correct := proxyNpmArchive(t, "1.0.0")
			wrong := proxyNpmArchive(t, "1.0.1")
			sha512Correct := sha512.Sum512(correct)
			sha512Other := sha512.Sum512([]byte("other acceptable package"))
			sha1Wrong := sha1.Sum(wrong)
			dist := ""
			if scheme == "multi-token SRI" {
				// A matching weak token must not override stronger mismatches;
				// either of the two strongest tokens may accept the body.
				integrity := fmt.Sprintf("sha1-%s sha512-%s sha512-%s",
					base64.StdEncoding.EncodeToString(sha1Wrong[:]),
					base64.StdEncoding.EncodeToString(sha512Other[:]),
					base64.StdEncoding.EncodeToString(sha512Correct[:]))
				dist = fmt.Sprintf(`"integrity":%q`, integrity)
			} else {
				sha1Correct := sha1.Sum(correct)
				dist = fmt.Sprintf(`"shasum":%q`, hex.EncodeToString(sha1Correct[:]))
			}
			bad := true
			tarballCalls := 0
			fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "registry.example":
					packument := fmt.Sprintf(`{"versions":{"1.0.0":{"dist":{"tarball":"https://tar.example/pkg-1.0.0.tgz",%s}}}}`, dist)
					return testHTTPResponse(request, http.StatusOK, packument), nil
				case "tar.example":
					tarballCalls++
					if bad {
						return testHTTPResponse(request, http.StatusOK, string(wrong)), nil
					}
					return testHTTPResponse(request, http.StatusOK, string(correct)), nil
				default:
					return testHTTPResponse(request, http.StatusNotFound, ""), nil
				}
			})})
			createTestRepository(t, fixture, domain.Repository{
				Name: "npm-integrity", Format: "npm", Type: "proxy", Upstream: "https://registry.example",
			})
			index := fixture.request(t, http.MethodGet, "/repository/npm-integrity/pkg", nil, true)
			assertStatus(t, index, http.StatusOK)
			index.Body.Close()
			path := "/repository/npm-integrity/pkg/-/pkg-1.0.0.tgz"
			failed := fixture.request(t, http.MethodGet, path, nil, true)
			assertStatus(t, failed, http.StatusBadGateway)
			failed.Body.Close()
			if _, err := fixture.Handler.blobs.Head(context.Background(), testDigest(wrong)); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("mismatched tarball was persisted: %v", err)
			}
			bad = false
			accepted := fixture.request(t, http.MethodGet, path, nil, true)
			assertStatus(t, accepted, http.StatusOK)
			assertBody(t, accepted, correct)
			cached := fixture.request(t, http.MethodGet, path, nil, true)
			assertStatus(t, cached, http.StatusOK)
			assertBody(t, cached, correct)
			if tarballCalls != 2 {
				t.Fatalf("upstream tarball calls = %d, want rejection followed by one cache fill", tarballCalls)
			}
		})
	}
}

func TestNpmProxyWarmTarballSurvivesPackumentDeletion(t *testing.T) {
	fixture := newServerFixture(t)
	createTestRepository(t, fixture, domain.Repository{
		Name: "npm-warm", Format: "npm", Type: "proxy", Upstream: "https://registry.example",
	})
	archive := proxyNpmArchive(t, "1.0.0")
	online := true
	tarballCalls := 0
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if !online {
			return testHTTPResponse(request, http.StatusServiceUnavailable, "offline"), nil
		}
		if request.URL.Path == "/pkg" {
			return testHTTPResponse(request, http.StatusOK,
				`{"versions":{"1.0.0":{"dist":{"tarball":"https://cdn.example/pkg.tgz"}}}}`), nil
		}
		tarballCalls++
		return testHTTPResponse(request, http.StatusOK, string(archive)), nil
	})})
	for _, path := range []string{"pkg", "pkg/-/pkg-1.0.0.tgz"} {
		response := fixture.request(t, http.MethodGet, "/repository/npm-warm/"+path, nil, true)
		assertStatus(t, response, http.StatusOK)
		response.Body.Close()
	}
	packument, err := fixture.Metadata.Asset(context.Background(), "npm-warm", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	deleted := fixture.request(t, http.MethodDelete,
		fmt.Sprintf("/api/v1/repositories/npm-warm/assets/%d", packument.ID), nil, true)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
	online = false
	response := fixture.request(t, http.MethodGet, "/repository/npm-warm/pkg/-/pkg-1.0.0.tgz", nil, true)
	assertStatus(t, response, http.StatusOK)
	assertBody(t, response, archive)
	if tarballCalls != 1 {
		t.Fatalf("tarball upstream calls = %d, want one cache fill", tarballCalls)
	}
}

func TestNpmProxyDoesNotChooseAmbiguousHistoricalTarball(t *testing.T) {
	fixture := newServerFixture(t)
	fixture.Handler.cfg.ProxyManifestTTL = 0
	fixture.Handler.content.SetConfig(fixture.Handler.cfg)
	createTestRepository(t, fixture, domain.Repository{
		Name: "npm-history", Format: "npm", Type: "proxy", Upstream: "https://registry.example",
	})
	archives := [][]byte{proxyNpmArchive(t, "1.0.0"), proxyNpmArchive(t, "1.0.1")}
	current := 0
	online := true
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if !online {
			return testHTTPResponse(request, http.StatusServiceUnavailable, "offline"), nil
		}
		if request.URL.Path == "/pkg" {
			digest := sha512.Sum512(archives[current])
			packument := fmt.Sprintf(`{"versions":{"1.0.0":{"dist":{"tarball":"https://cdn%d.example/pkg.tgz","integrity":"sha512-%s"}}}}`,
				current, base64.StdEncoding.EncodeToString(digest[:]))
			return testHTTPResponse(request, http.StatusOK, packument), nil
		}
		return testHTTPResponse(request, http.StatusOK, string(archives[current])), nil
	})})
	for generation := range archives {
		current = generation
		index := fixture.request(t, http.MethodGet, "/repository/npm-history/pkg", nil, true)
		assertStatus(t, index, http.StatusOK)
		index.Body.Close()
		tarball := fixture.request(t, http.MethodGet, "/repository/npm-history/pkg/-/pkg-1.0.0.tgz", nil, true)
		assertStatus(t, tarball, http.StatusOK)
		assertBody(t, tarball, archives[generation])
	}
	packument, err := fixture.Metadata.Asset(context.Background(), "npm-history", "pkg")
	if err != nil {
		t.Fatal(err)
	}
	deleted := fixture.request(t, http.MethodDelete,
		fmt.Sprintf("/api/v1/repositories/npm-history/assets/%d", packument.ID), nil, true)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
	online = false
	response := fixture.request(t, http.MethodGet, "/repository/npm-history/pkg/-/pkg-1.0.0.tgz", nil, true)
	assertStatus(t, response, http.StatusBadGateway)
	response.Body.Close()
}
