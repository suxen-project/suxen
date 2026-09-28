package suxencmd

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/startup"
)

func TestInitializeStopsOnPermanentFailure(t *testing.T) {
	cause := errors.New("invalid desired state")
	attempts := 0
	ready, err := initializeWithAttempt(func() error {
		attempts++
		return cause
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, time.Millisecond, time.Second)
	if ready || !errors.Is(err, cause) || attempts != 1 {
		t.Fatalf("ready=%v err=%v attempts=%d, want permanent failure after one attempt", ready, err, attempts)
	}
}

func TestInitializeRetriesMarkedDependencyFailure(t *testing.T) {
	cause := errors.New("database unavailable")
	attempts := 0
	ready, err := initializeWithAttempt(func() error {
		attempts++
		if attempts == 1 {
			return startup.Retry(cause)
		}
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, time.Millisecond, time.Second)
	if !ready || err != nil || attempts != 2 {
		t.Fatalf("ready=%v err=%v attempts=%d, want success after retry", ready, err, attempts)
	}
}

func TestInvalidLogLevelWarnsAndFallsBackToInfo(t *testing.T) {
	var output bytes.Buffer
	logger := newLoggerWithOutput("extremely-loud", &output)
	logger.Debug("debug record must remain filtered")
	logger.Info("info record remains enabled")

	logs := output.String()
	if !strings.Contains(logs, `"msg":"invalid log level; using info"`) {
		t.Fatalf("invalid level warning is missing: %s", logs)
	}
	if !strings.Contains(logs, `"configured_level":"extremely-loud"`) {
		t.Fatalf("invalid configured level is missing: %s", logs)
	}
	if strings.Contains(logs, "debug record must remain filtered") {
		t.Fatalf("fallback logger enabled debug output: %s", logs)
	}
	if !strings.Contains(logs, "info record remains enabled") {
		t.Fatalf("fallback logger did not enable info output: %s", logs)
	}
}

func TestShutdownHTTPServerDrainsAnInFlightUpload(t *testing.T) {
	uploadStarted := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(uploadStarted)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- httpServer.Serve(listener)
	}()
	t.Cleanup(func() { _ = httpServer.Close() })

	bodyReader, bodyWriter := io.Pipe()
	t.Cleanup(func() { _ = bodyWriter.Close() })
	type clientResult struct {
		response *http.Response
		err      error
	}
	clientDone := make(chan clientResult, 1)
	go func() {
		request, requestErr := http.NewRequest(
			http.MethodPut,
			"http://"+listener.Addr().String()+"/repository/raw/in-flight.bin",
			bodyReader,
		)
		if requestErr != nil {
			clientDone <- clientResult{err: requestErr}
			return
		}
		response, requestErr := http.DefaultClient.Do(request)
		clientDone <- clientResult{response: response, err: requestErr}
	}()

	select {
	case <-uploadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upload handler did not start")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- shutdownHTTPServer(httpServer, 2*time.Second)
	}()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned before the upload drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := bodyWriter.Write([]byte("completed upload")); err != nil {
		t.Fatal(err)
	}
	if err := bodyWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-clientDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		defer result.response.Body.Close()
		if result.response.StatusCode != http.StatusCreated {
			t.Fatalf("upload status = %d, want 201", result.response.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not complete")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after the upload completed")
	}
	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve returned %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP server did not stop")
	}
}
