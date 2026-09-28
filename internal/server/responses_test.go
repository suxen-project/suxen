package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func TestWriteProblemUsesProblemJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	httpx.WriteProblem(
		recorder,
		http.StatusBadRequest,
		"invalid_request",
		"the request is invalid",
	)

	response := recorder.Result()
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", contentType)
	}
	var problem httpx.ProblemDetails
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type != "urn:suxen:problem:invalid_request" ||
		problem.Title != "Invalid request" ||
		problem.Status != http.StatusBadRequest ||
		problem.Detail != "the request is invalid" ||
		problem.Code != "invalid_request" {
		t.Fatalf("unexpected problem details: %+v", problem)
	}
}

func TestControlPlaneRouteUsesProblemJSON(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodGet, "/api/v1/missing", nil, true)
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", contentType)
	}
	var problem httpx.ProblemDetails
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "not_found" {
		t.Fatalf("problem code = %q, want not_found", problem.Code)
	}
}

func TestWriteResultMapsKnownAndInternalErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantDetail string
	}{
		{
			name:       "not found",
			err:        domain.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
			wantDetail: domain.ErrNotFound.Error(),
		},
		{
			name:       "read only",
			err:        domain.ErrReadOnly,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "read_only",
			wantDetail: domain.ErrReadOnly.Error(),
		},
		{
			name:       "digest mismatch",
			err:        domain.ErrDigestMismatch,
			wantStatus: http.StatusBadRequest,
			wantCode:   "digest_mismatch",
			wantDetail: domain.ErrDigestMismatch.Error(),
		},
		{
			name:       "validation",
			err:        domain.ErrInvalidRepository,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_repository",
			wantDetail: domain.ErrInvalidRepository.Error(),
		},
		{
			name:       "forbidden",
			err:        domain.ErrProvenanceRejected,
			wantStatus: http.StatusForbidden,
			wantCode:   "provenance_rejected",
			wantDetail: domain.ErrProvenanceRejected.Error(),
		},
		{
			name:       "internal",
			err:        errors.New("database password must not escape"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
			wantDetail: "internal server error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			httpx.WriteResult(recorder, nil, test.err)

			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
				t.Fatalf("Content-Type = %q, want application/problem+json", contentType)
			}
			var problem httpx.ProblemDetails
			if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
				t.Fatal(err)
			}
			if problem.Code != test.wantCode || problem.Detail != test.wantDetail {
				t.Fatalf("unexpected problem details: %+v", problem)
			}
		})
	}
}

func TestWriteResultUsesOCIErrorMappings(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "not found",
			err:        domain.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "NAME_UNKNOWN",
		},
		{
			name:       "read only",
			err:        domain.ErrReadOnly,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "UNSUPPORTED",
		},
		{
			name:       "digest mismatch",
			err:        domain.ErrDigestMismatch,
			wantStatus: http.StatusBadRequest,
			wantCode:   "DIGEST_INVALID",
		},
		{
			name:       "forbidden",
			err:        domain.ErrProvenanceRejected,
			wantStatus: http.StatusForbidden,
			wantCode:   "DENIED",
		},
		{
			name:       "internal",
			err:        errors.New("storage credentials must not escape"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "UNKNOWN",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tracked := &httpx.StatusWriter{ResponseWriter: recorder}
			httpx.SelectErrorResponseFormat(tracked, httpx.ErrorResponseFormatOCI)
			httpx.WriteResult(tracked, nil, test.err)

			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", contentType)
			}
			var envelope httpx.OCIErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Errors) != 1 || envelope.Errors[0].Code != test.wantCode {
				t.Fatalf("unexpected OCI error envelope: %+v", envelope)
			}
			if envelope.Errors[0].Message == test.err.Error() && test.wantStatus == http.StatusInternalServerError {
				t.Fatal("internal error detail escaped into OCI response")
			}
		})
	}
}

func TestWriteServerProblemLogsCauseAndRedactsResponse(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	recorder := httptest.NewRecorder()
	tracked := &httpx.StatusWriter{
		ResponseWriter: recorder,
		Log:            logger,
	}
	cause := errors.New("backend failure marker")

	httpx.WriteServerProblem(
		tracked,
		http.StatusInternalServerError,
		"authorization_error",
		"authorization failed",
		cause,
	)

	response := recorder.Result()
	defer response.Body.Close()
	var problem httpx.ProblemDetails
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Detail != "authorization failed" {
		t.Fatalf("problem detail = %q, want redacted public detail", problem.Detail)
	}
	if strings.Contains(problem.Detail, cause.Error()) {
		t.Fatal("internal error detail escaped into problem response")
	}
	if !strings.Contains(logs.String(), cause.Error()) {
		t.Fatalf("internal error was not logged: %s", logs.String())
	}
}

func TestDefaultOCIErrorUsesDistributionEnvelope(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(t, http.MethodOptions, "/v2/example/blobs/sha256:bad", nil, false)
	defer response.Body.Close()

	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var envelope struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "UNSUPPORTED" {
		t.Fatalf("unexpected OCI error envelope: %+v", envelope)
	}
}

func TestDefaultOCIAuthenticationErrorUsesDistributionEnvelope(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(
		t,
		http.MethodPut,
		"/v2/example/manifests/latest",
		[]byte("{}"),
		false,
	)
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var envelope httpx.OCIErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "UNAUTHORIZED" {
		t.Fatalf("unexpected OCI error envelope: %+v", envelope)
	}
}

func TestPrefixedOCIRouteSelectsDistributionErrorsAfterRepositoryResolution(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	tests := []struct {
		name          string
		method        string
		path          string
		wantStatus    int
		wantErrorCode string
		wantProblem   bool
	}{
		{
			name:          "unknown repository",
			method:        http.MethodGet,
			path:          "/repository/missing/v2/example/manifests/latest",
			wantStatus:    http.StatusNotFound,
			wantErrorCode: "not_found",
			wantProblem:   true,
		},
		{
			name:          "authentication required",
			method:        http.MethodPut,
			path:          "/repository/oci/v2/example/manifests/latest",
			wantStatus:    http.StatusUnauthorized,
			wantErrorCode: "UNAUTHORIZED",
		},
		{
			name:          "unsupported method",
			method:        http.MethodOptions,
			path:          "/repository/oci/v2/example/manifests/latest",
			wantStatus:    http.StatusMethodNotAllowed,
			wantErrorCode: "UNSUPPORTED",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.request(t, test.method, test.path, nil, false)
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if test.wantProblem {
				if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
					t.Fatalf("Content-Type = %q, want application/problem+json", contentType)
				}
				var problem httpx.ProblemDetails
				if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
					t.Fatal(err)
				}
				if problem.Code != test.wantErrorCode {
					t.Fatalf("unexpected problem details: %+v", problem)
				}
				return
			}
			if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", contentType)
			}
			var envelope httpx.OCIErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if len(envelope.Errors) != 1 || envelope.Errors[0].Code != test.wantErrorCode {
				t.Fatalf("unexpected OCI error envelope: %+v", envelope)
			}
		})
	}
}

func TestRawV2ShapedPathKeepsProblemDetailsEnvelope(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(
		t,
		http.MethodGet,
		"/repository/raw/v2/example/manifests/latest",
		nil,
		true,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", contentType)
	}
	var problem httpx.ProblemDetails
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "not_found" {
		t.Fatalf("unexpected problem details: %+v", problem)
	}
}

func TestStatusWriterUnwrapsForResponseController(t *testing.T) {
	recorder := httptest.NewRecorder()
	tracked := &httpx.StatusWriter{ResponseWriter: recorder}

	if err := http.NewResponseController(tracked).Flush(); err != nil {
		t.Fatalf("flush through status writer: %v", err)
	}
}

func TestWriteRequestDTOsRejectReadOnlyAndPathAuthoritativeFields(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		destination func() any
	}{
		{name: "repository create createdAt", body: `{"name":"raw","format":"raw","type":"hosted","createdAt":"2026-01-01T00:00:00Z"}`, destination: func() any { return &repositoryRequest{} }},
		{name: "repository update name", body: `{"name":"ignored","format":"raw","type":"hosted"}`, destination: func() any { return &repositoryUpdateRequest{} }},
		{name: "blob store create createdAt", body: `{"name":"archive","driver":"fs","configurationRef":{"env":"SUXEN_ARCHIVE"},"createdAt":"2026-01-01T00:00:00Z"}`, destination: func() any { return &blobStoreResourceRequest{} }},
		{name: "blob store update name", body: `{"name":"ignored","driver":"fs","configurationRef":{"env":"SUXEN_ARCHIVE"}}`, destination: func() any { return &blobStoreUpdateRequest{} }},
		{name: "role create createdAt", body: `{"name":"reader","privileges":[],"createdAt":"2026-01-01T00:00:00Z"}`, destination: func() any { return &roleRequest{} }},
		{name: "role update name", body: `{"name":"ignored","privileges":[]}`, destination: func() any { return &roleUpdateRequest{} }},
		{name: "user create createdAt", body: `{"username":"reader","password":"secret","createdAt":"2026-01-01T00:00:00Z"}`, destination: func() any { return &userRequest{} }},
		{name: "user update username", body: `{"username":"ignored","admin":false}`, destination: func() any { return &userUpdateRequest{} }},
		{name: "OIDC create createdAt", body: `{"name":"main","issuer":"https://id.example","clientId":"suxen","createdAt":"2026-01-01T00:00:00Z"}`, destination: func() any { return &oidcProviderRequest{} }},
		{name: "OIDC update name", body: `{"name":"ignored","issuer":"https://id.example","clientId":"suxen"}`, destination: func() any { return &oidcProviderUpdateRequest{} }},
		{name: "cleanup create createdAt", body: `{"name":"old","repositories":["raw"],"criteria":[{"path":"sys.path","op":"exists"}],"createdAt":"2026-01-01T00:00:00Z"}`, destination: func() any { return &cleanupPolicyRequest{} }},
		{name: "cleanup update name", body: `{"name":"ignored","repositories":["raw"],"criteria":[{"path":"sys.path","op":"exists"}]}`, destination: func() any { return &cleanupPolicyUpdateRequest{} }},
		{name: "classification repository", body: `{"repository":"ignored","rules":[]}`, destination: func() any { return &classificationRequest{} }},
		{name: "gate repository", body: `{"repository":"ignored","criteria":[{"path":"sys.path","op":"exists"}]}`, destination: func() any { return &downloadGateRequest{} }},
		{name: "trust policy repository", body: `{"repository":"ignored","mode":"audit","publicKeys":[]}`, destination: func() any { return &trustPolicyRequest{} }},
		{name: "webhook updatedAt", body: `{"name":"scanner","url":"https://scanner.example","events":["asset.uploaded"],"updatedAt":"2026-01-01T00:00:00Z"}`, destination: func() any { return &webhookRequest{} }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			if httpx.DecodeJSON(response, request, test.destination()) {
				t.Fatal("decodeJSON accepted a field outside the request DTO")
			}
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.Code)
			}
		})
	}
}

func TestDecodeJSONRejectsNullAndTrailingValues(t *testing.T) {
	tests := []string{
		"null",
		`{"name":"first"} {"name":"second"}`,
		`{"name":"first"} trailing`,
	}

	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			response := httptest.NewRecorder()
			var destination struct {
				Name string `json:"name"`
			}
			if httpx.DecodeJSON(response, request, &destination) {
				t.Fatal("decodeJSON accepted an invalid request body")
			}
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.Code)
			}
			var problem struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if problem.Code != "invalid_json" {
				t.Errorf("problem code = %q, want invalid_json", problem.Code)
			}
		})
	}
}

func TestDecodeJSONAcceptsOneValueWithTrailingWhitespace(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader("{\"name\":\"only\"}\n\t"),
	)
	response := httptest.NewRecorder()
	var destination struct {
		Name string `json:"name"`
	}
	if !httpx.DecodeJSON(response, request, &destination) {
		t.Fatalf("decodeJSON rejected valid body: %s", response.Body.String())
	}
	if destination.Name != "only" {
		t.Errorf("decoded name = %q, want only", destination.Name)
	}
}

func TestDecodeJSONPreservesExactNumbersInUntypedFields(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"value":9007199254740993,"nested":{"fraction":1.25}}`,
	))
	response := httptest.NewRecorder()
	var destination struct {
		Value  any            `json:"value"`
		Nested map[string]any `json:"nested"`
	}
	if !httpx.DecodeJSON(response, request, &destination) {
		t.Fatalf("DecodeJSON rejected valid body: %s", response.Body.String())
	}
	if destination.Value != json.Number("9007199254740993") ||
		destination.Nested["fraction"] != json.Number("1.25") {
		t.Fatalf("decoded values = %+v", destination)
	}
	encoded, err := json.Marshal(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`9007199254740993`)) {
		t.Fatalf("JSON output rounded the integer: %s", encoded)
	}
}

func TestClassificationNumberSurvivesHTTPAndSQLRoundTrip(t *testing.T) {
	fixture := newServerFixture(t)
	path := "/api/v1/repositories/raw/classification"
	put := fixture.requestWithBearer(t, http.MethodPut, path,
		[]byte(`{"rules":[{"when":[{"path":"sys.size","op":">","value":9007199254740993}],"key":"size","value":"large"}]}`),
		"application/json", testToken)
	assertStatus(t, put, http.StatusOK)
	put.Body.Close()

	get := fixture.request(t, http.MethodGet, path, nil, true)
	defer get.Body.Close()
	assertStatus(t, get, http.StatusOK)
	var stored domain.ClassificationConfig
	decoder := json.NewDecoder(get.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Rules[0].When[0].Value; got != json.Number("9007199254740993") {
		t.Fatalf("stored policy number = %v, want 9007199254740993", got)
	}
}

func TestNonpositiveIDsReturnInvalidID(t *testing.T) {
	fixture := newServerFixture(t)
	for _, trial := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/repositories/raw/assets/0/attributes/scan"},
		{http.MethodGet, "/api/v1/repositories/raw/assets/-1/attributes/scan"},
		{http.MethodGet, "/api/v1/tasks/0"},
		{http.MethodGet, "/api/v1/tasks/-1"},
		{http.MethodDelete, "/api/v1/users/admin/tokens/0"},
		{http.MethodDelete, "/api/v1/users/admin/tokens/-1"},
	} {
		t.Run(trial.path, func(t *testing.T) {
			response := fixture.request(t, trial.method, trial.path, nil, true)
			defer response.Body.Close()
			assertStatus(t, response, http.StatusBadRequest)
			var problem httpx.ProblemDetails
			if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
				t.Fatal(err)
			}
			if problem.Code != "invalid_id" {
				t.Fatalf("problem code = %q, want invalid_id", problem.Code)
			}
		})
	}
}

func TestDecodeJSONRejectsBodiesOverLimit(t *testing.T) {
	body := `{"value":"` + strings.Repeat("x", (1<<20)+1) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	response := httptest.NewRecorder()
	var destination map[string]any
	if httpx.DecodeJSON(response, request, &destination) {
		t.Fatal("decodeJSON accepted an oversized request body")
	}
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}
