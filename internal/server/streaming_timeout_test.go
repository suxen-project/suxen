package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRawUploadAcceptsSlowStreamingRequest(t *testing.T) {
	fixture := newServerFixture(t)
	server := httptest.NewUnstartedServer(fixture.Handler)
	server.Config.ReadHeaderTimeout = 10 * time.Second
	server.Config.ReadTimeout = fixture.Handler.cfg.ReadTimeout
	server.Config.WriteTimeout = fixture.Handler.cfg.WriteTimeout
	server.Start()
	defer server.Close()

	reader, writer := io.Pipe()
	written := make(chan error, 1)
	go func() {
		if _, err := writer.Write([]byte("slow ")); err != nil {
			written <- err
			return
		}
		time.Sleep(90 * time.Millisecond)
		_, err := writer.Write([]byte("upload"))
		if err == nil {
			err = writer.Close()
		}
		written <- err
	}()
	request, err := http.NewRequest(http.MethodPut, server.URL+"/repository/raw/slow-stream.txt", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("slow upload status = %d, want 201", response.StatusCode)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	get, err := http.NewRequest(http.MethodGet, server.URL+"/repository/raw/slow-stream.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	get.Header.Set("Authorization", "Bearer "+testToken)
	response, err = client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "slow upload" {
		t.Fatalf("streamed asset = %q, %v", body, err)
	}
}
