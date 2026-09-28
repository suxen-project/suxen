// Command e2e-support provides deterministic external systems for black-box tests.
// It intentionally uses only the Go standard library so its test image stays small.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maximumRequestBytes = 32 << 20

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: e2e-support <check|health|raw-upstream|registry-token|webhook-sink|webhook-enricher>")
	}

	var err error
	switch os.Args[1] {
	case "check":
		return
	case "health":
		err = checkHealth()
	case "raw-upstream":
		err = serveRawUpstream()
	case "registry-token":
		err = serveRegistryTokenIssuer()
	case "webhook-sink":
		err = serveWebhookSink()
	case "webhook-enricher":
		err = serveWebhookEnricher()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func checkHealth() error {
	if len(os.Args) != 3 {
		return errors.New("usage: e2e-support health <url>")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(os.Args[2])
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %s", response.Status)
	}
	return nil
}

func serveRawUpstream() error {
	stateDirectory := environment("E2E_STATE", "/state")
	if err := os.MkdirAll(filepath.Join(stateDirectory, "files"), 0o750); err != nil {
		return fmt.Errorf("create raw upstream state: %w", err)
	}

	upstream := &rawUpstream{
		root:     filepath.Join(stateDirectory, "files"),
		username: environment("E2E_USERNAME", "upstream"),
		password: environment("E2E_PASSWORD", "upstream-password"),
		hits:     make(map[string]int),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /__hits", upstream.handleHits)
	mux.HandleFunc("DELETE /__hits", upstream.handleResetHits)
	mux.HandleFunc("/files/", upstream.handleFile)
	return listen(mux)
}

type rawUpstream struct {
	root     string
	username string
	password string
	mu       sync.Mutex
	hits     map[string]int
}

func (upstream *rawUpstream) handleFile(w http.ResponseWriter, request *http.Request) {
	if !upstream.authorized(request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="raw-upstream"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	relativePath := strings.TrimPrefix(request.URL.Path, "/files/")
	filePath, err := safeFilePath(upstream.root, relativePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	upstream.mu.Lock()
	upstream.hits[relativePath]++
	upstream.mu.Unlock()

	switch request.Method {
	case http.MethodPut:
		upstream.putFile(w, request, filePath)
	case http.MethodGet, http.MethodHead:
		http.ServeFile(w, request, filePath)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (upstream *rawUpstream) authorized(request *http.Request) bool {
	username, password, ok := request.BasicAuth()
	if !ok {
		return false
	}
	usernameMatch := subtle.ConstantTimeCompare([]byte(username), []byte(upstream.username))
	passwordMatch := subtle.ConstantTimeCompare([]byte(password), []byte(upstream.password))
	return usernameMatch&passwordMatch == 1
}

func (upstream *rawUpstream) putFile(
	w http.ResponseWriter,
	request *http.Request,
	filePath string,
) {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
		http.Error(w, "create parent directory", http.StatusInternalServerError)
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(filePath), ".upload-*")
	if err != nil {
		http.Error(w, "create temporary upload", http.StatusInternalServerError)
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	limited := http.MaxBytesReader(w, request.Body, maximumRequestBytes)
	if _, err := io.Copy(temporary, limited); err != nil {
		_ = temporary.Close()
		http.Error(w, "store upload", http.StatusBadRequest)
		return
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		http.Error(w, "sync upload", http.StatusInternalServerError)
		return
	}
	if err := temporary.Close(); err != nil {
		http.Error(w, "close upload", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		http.Error(w, "commit upload", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (upstream *rawUpstream) handleHits(w http.ResponseWriter, request *http.Request) {
	relativePath := request.URL.Query().Get("path")
	upstream.mu.Lock()
	count := upstream.hits[relativePath]
	upstream.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"path": relativePath, "count": count})
}

func (upstream *rawUpstream) handleResetHits(w http.ResponseWriter, _ *http.Request) {
	upstream.mu.Lock()
	upstream.hits = make(map[string]int)
	upstream.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func safeFilePath(root string, relativePath string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(relativePath))
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid file path")
	}
	return filepath.Join(root, cleaned), nil
}

// serveWebhookEnricher models an external scanner: it receives asset webhooks
// and stamps an attribute back onto the asset through the suxen API, using a
// scoped service-account token (repository:<name>:annotate). This is the
// supported "webhook -> attribute" integration; suxen does not read attributes
// from the webhook response, so the receiver calls the API itself.
func serveWebhookEnricher() error {
	suxenURL := strings.TrimRight(environment("SUXEN_URL", ""), "/")
	token := os.Getenv("SUXEN_TOKEN")
	if suxenURL == "" || token == "" {
		return errors.New("webhook-enricher requires SUXEN_URL and SUXEN_TOKEN")
	}
	value := environment("E2E_ATTR_VALUE", `{"status":"passed"}`)
	if !json.Valid([]byte(value)) {
		return fmt.Errorf("E2E_ATTR_VALUE is not JSON: %q", value)
	}
	enricher := &webhookEnricher{
		secret:    environment("E2E_SECRET", "webhook-test-secret"),
		suxenURL:  suxenURL,
		token:     token,
		namespace: environment("E2E_ATTR_NAMESPACE", "scan"),
		value:     []byte(value),
		client:    &http.Client{Timeout: 10 * time.Second},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("POST /webhook", enricher.handleWebhook)
	return listen(mux)
}

type webhookEnricher struct {
	secret    string
	suxenURL  string
	token     string
	namespace string
	value     []byte
	client    *http.Client
}

type enricherEvent struct {
	Type       string `json:"type"`
	Repository string `json:"repository"`
	Asset      *struct {
		ID     int64  `json:"id"`
		Path   string `json:"path"`
		Digest string `json:"digest"`
	} `json:"asset"`
}

func (e *webhookEnricher) handleWebhook(w http.ResponseWriter, request *http.Request) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 1<<20))
	if err != nil {
		http.Error(w, "read payload", http.StatusBadRequest)
		return
	}
	mac := hmac.New(sha256.New, []byte(e.secret))
	_, _ = mac.Write(payload)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(request.Header.Get("X-Suxen-Signature-256"))) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	var event enricherEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		http.Error(w, "decode event", http.StatusBadRequest)
		return
	}
	if event.Asset == nil || event.Repository == "" {
		// No asset to annotate (e.g. a delete event); acknowledge so it is not
		// retried.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.TrimSpace(event.Asset.Digest) == "" {
		http.Error(w, "asset digest is required", http.StatusBadRequest)
		return
	}
	if err := e.stamp(request.Context(), event); err != nil {
		// A 5xx makes suxen retry the delivery (at-least-once).
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("stamped %s on %s/assets/%d (%s)", e.namespace, event.Repository, event.Asset.ID, event.Asset.Path)
	w.WriteHeader(http.StatusNoContent)
}

func (e *webhookEnricher) stamp(ctx context.Context, event enricherEvent) error {
	url := fmt.Sprintf(
		"%s/api/v1/repositories/%s/assets/%d/attributes/%s",
		e.suxenURL, event.Repository, event.Asset.ID, e.namespace,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(e.value))
	if err != nil {
		return fmt.Errorf("build attribute request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+e.token)
	request.Header.Set("If-Match", event.Asset.Digest)
	response, err := e.client.Do(request)
	if err != nil {
		return fmt.Errorf("call suxen: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("suxen attribute PUT %s: %s", response.Status, bytes.TrimSpace(body))
	}
	return nil
}

func serveWebhookSink() error {
	stateDirectory := environment("E2E_STATE", "/state")
	if err := os.MkdirAll(stateDirectory, 0o750); err != nil {
		return fmt.Errorf("create webhook state: %w", err)
	}

	sink := &webhookSink{
		secret: environment("E2E_SECRET", "webhook-test-secret"),
		path:   filepath.Join(stateDirectory, "events.jsonl"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /events", sink.handleEvents)
	mux.HandleFunc("DELETE /events", sink.handleReset)
	mux.HandleFunc("POST /webhook", sink.handleWebhook)
	mux.HandleFunc("POST /fail", sink.handleFailure)
	return listen(mux)
}

type webhookSink struct {
	secret string
	path   string
	mu     sync.Mutex
}

type receivedWebhook struct {
	ReceivedAt time.Time       `json:"receivedAt"`
	Payload    json.RawMessage `json:"payload"`
}

func (sink *webhookSink) handleWebhook(w http.ResponseWriter, request *http.Request) {
	payload, valid := sink.verifiedPayload(w, request)
	if !valid {
		return
	}
	if err := sink.append(payload); err != nil {
		http.Error(w, "persist webhook", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (sink *webhookSink) handleFailure(w http.ResponseWriter, request *http.Request) {
	if _, valid := sink.verifiedPayload(w, request); !valid {
		return
	}
	http.Error(w, "intentional receiver failure", http.StatusServiceUnavailable)
}

func (sink *webhookSink) verifiedPayload(
	w http.ResponseWriter,
	request *http.Request,
) ([]byte, bool) {
	payload, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 1<<20))
	if err != nil {
		http.Error(w, "read payload", http.StatusBadRequest)
		return nil, false
	}
	if !json.Valid(payload) {
		http.Error(w, "payload is not JSON", http.StatusBadRequest)
		return nil, false
	}

	expectedMAC := hmac.New(sha256.New, []byte(sink.secret))
	_, _ = expectedMAC.Write(payload)
	expected := "sha256=" + hex.EncodeToString(expectedMAC.Sum(nil))
	provided := request.Header.Get("X-Suxen-Signature-256")
	if !hmac.Equal([]byte(expected), []byte(provided)) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return nil, false
	}
	return payload, true
}

func (sink *webhookSink) append(payload []byte) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()

	file, err := os.OpenFile(sink.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(receivedWebhook{
		ReceivedAt: time.Now().UTC(),
		Payload:    append(json.RawMessage(nil), payload...),
	})
}

func (sink *webhookSink) handleEvents(w http.ResponseWriter, _ *http.Request) {
	sink.mu.Lock()
	defer sink.mu.Unlock()

	file, err := os.Open(sink.path)
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, []receivedWebhook{})
		return
	}
	if err != nil {
		http.Error(w, "read webhook history", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	items := make([]receivedWebhook, 0)
	decoder := json.NewDecoder(file)
	for {
		var item receivedWebhook
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			http.Error(w, "decode webhook history", http.StatusInternalServerError)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func (sink *webhookSink) handleReset(w http.ResponseWriter, _ *http.Request) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if err := os.Remove(sink.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "reset webhook history", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func listen(handler http.Handler) error {
	server := &http.Server{
		Addr:              environment("E2E_LISTEN", ":8080"),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("e2e support listening on %s", server.Addr)
	return server.ListenAndServe()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func environment(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fatalf(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(1)
}
