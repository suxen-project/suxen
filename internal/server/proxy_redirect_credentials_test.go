package server

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/outbound"
)

func TestProxyRedirectChainKeepsCredentialsAtOriginalPort(t *testing.T) {
	t.Parallel()
	credentials := make(chan string, 3)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentials <- r.Header.Get("Authorization")
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/artifact", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "proxied artifact")
	}))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentials <- r.Header.Get("Authorization")
		http.Redirect(w, r, destination.URL+"/first", http.StatusFound)
	}))
	defer upstream.Close()

	fixture := newServerFixture(t)
	client := outbound.NewPolicy(outbound.Options{AllowedHosts: []string{"127.0.0.1"}}).Client(time.Second * 10)
	defer client.CloseIdleConnections()
	fixture.Handler.setHTTPClient(client)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	target.User = url.UserPassword("upstream-user", "synthetic-upstream-password")
	if err := fixture.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: "redirect-chain", Format: "raw", Type: "proxy", Upstream: target.String(),
	}); err != nil {
		t.Fatal(err)
	}
	response := fixture.request(t, http.MethodGet, "/repository/redirect-chain/artifact", nil, true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "proxied artifact" {
		t.Fatalf("proxy body = %q, error = %v", body, err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("upstream-user:synthetic-upstream-password"))
	for hop := 0; hop < 3; hop++ {
		select {
		case got := <-credentials:
			if got != want {
				t.Errorf("hop %d Authorization = %q, want %q", hop, got, want)
			}
		default:
			t.Fatalf("missing request for hop %d", hop)
		}
		want = ""
	}
}
