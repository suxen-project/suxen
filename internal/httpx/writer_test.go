package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"reflect"
	"testing"
)

func TestStatusWriterPreservesInformationalResponses(t *testing.T) {
	status := make(chan int, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := NewStatusWriter(w, nil, nil)
		tracked.WriteHeader(http.StatusEarlyHints)
		tracked.WriteHeader(http.StatusProcessing)
		tracked.Header().Set("X-Final", "yes")
		tracked.WriteHeader(http.StatusCreated)
		_, _ = tracked.Write([]byte("created"))
		status <- tracked.Status()
	}))
	defer server.Close()

	var hints []int
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			hints = append(hints, code)
			return nil
		},
	}))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hints, []int{http.StatusEarlyHints, http.StatusProcessing}) {
		t.Errorf("informational responses = %v", hints)
	}
	if response.StatusCode != http.StatusCreated || response.Header.Get("X-Final") != "yes" || string(body) != "created" {
		t.Errorf("final response = %d %v %q", response.StatusCode, response.Header, body)
	}
	if got := <-status; got != response.StatusCode {
		t.Errorf("recorded status = %d, wire status = %d", got, response.StatusCode)
	}
}

func TestStatusWriterTracksImplicitResponse(t *testing.T) {
	for _, action := range []string{"write", "flush"} {
		t.Run(action, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tracked := NewStatusWriter(recorder, nil, nil)
			if action == "write" {
				_, _ = tracked.Write([]byte("body"))
			} else if err := http.NewResponseController(tracked).Flush(); err != nil {
				t.Fatal(err)
			}
			tracked.WriteHeader(http.StatusInternalServerError)
			if got := tracked.Status(); got != recorder.Code || got != http.StatusOK {
				t.Errorf("recorded status = %d, wire status = %d; want 200", got, recorder.Code)
			}
		})
	}
}
