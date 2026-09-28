package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	_ "github.com/suxen-project/suxen/plugins/format/pypi"
)

func TestPyPIDownloadsRequireAnAdvertisedExactURL(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRepository(ctx, domain.Repository{
		Name:     "review-pypi",
		Format:   "pypi",
		Type:     "proxy",
		Upstream: "https://mirror-user:mirror-secret@mirror.example/team-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Metadata.UpdateRole(ctx, domain.Role{
		Name: "anonymous", Privileges: []string{"repository:review-pypi:read"},
	}); err != nil {
		t.Fatal(err)
	}

	var requests []string
	fixture.Handler.setHTTPClient(&http.Client{Transport: roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		requests = append(requests, request.URL.String())
		switch request.URL.String() {
		case "https://mirror.example/team-a/simple/widget":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body: io.NopCloser(strings.NewReader(
					`<html><a href="https://cdn.example/releases/widget.whl?token=a%2Bb&part=one&part=two">widget.whl</a>` +
						`<a href="private.whl?access=required">private.whl</a></html>`,
				)),
				Request: request,
			}, nil
		case "https://mirror.example/team-a/simple/widget/private.whl?access=required":
			username, password, ok := request.BasicAuth()
			if !ok || username != "mirror-user" || password != "mirror-secret" {
				t.Fatal("same-origin advertised download did not receive repository credentials")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
				Body:       io.NopCloser(strings.NewReader("private wheel")),
				Request:    request,
			}, nil
		case "https://cdn.example/releases/widget.whl?token=a%2Bb&part=one&part=two":
			if _, _, ok := request.BasicAuth(); ok {
				t.Fatal("cross-origin advertised download received upstream credentials")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
				Body:       io.NopCloser(strings.NewReader("wheel")),
				Request:    request,
			}, nil
		default:
			t.Fatalf("unexpected upstream request %s", request.URL)
			return nil, nil
		}
	})})

	index := fixture.request(
		t, http.MethodGet, "/repository/review-pypi/simple/widget/", nil, false,
	)
	indexBody, _ := io.ReadAll(index.Body)
	index.Body.Close()
	assertStatus(t, index, http.StatusOK)
	if !strings.Contains(
		string(indexBody),
		"?token=a%2Bb&amp;part=one&amp;part=two",
	) {
		t.Fatalf("rewritten index lost signed query: %s", indexBody)
	}

	download := fixture.request(
		t,
		http.MethodGet,
		"/repository/review-pypi/files/https/cdn.example/releases/widget.whl?token=a%2Bb&part=one&part=two",
		nil,
		false,
	)
	assertStatus(t, download, http.StatusOK)
	assertBody(t, download, []byte("wheel"))
	privateDownload := fixture.request(
		t,
		http.MethodGet,
		"/repository/review-pypi/files/https/mirror.example/team-a/simple/widget/private.whl?access=required",
		nil,
		false,
	)
	assertStatus(t, privateDownload, http.StatusOK)
	assertBody(t, privateDownload, []byte("private wheel"))

	unadvertised := fixture.request(
		t,
		http.MethodGet,
		"/repository/review-pypi/files/https/mirror.example/admin/users",
		nil,
		false,
	)
	assertStatus(t, unadvertised, http.StatusBadRequest)
	unadvertisedBody, _ := io.ReadAll(unadvertised.Body)
	unadvertised.Body.Close()
	if !strings.Contains(string(unadvertisedBody), `"code":"pypi_unadvertised_file"`) {
		t.Fatalf("unadvertised response = %s", unadvertisedBody)
	}
	if len(requests) != 3 {
		t.Fatalf("unadvertised path reached upstream; requests = %v", requests)
	}
}
