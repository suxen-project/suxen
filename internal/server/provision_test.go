package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/blob"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/provision"
	"github.com/suxen-project/suxen/internal/startup"
	"github.com/suxen-project/suxen/internal/store"
)

type failingProvisionValidationStore struct{ store.Store }

func (failingProvisionValidationStore) OIDCProviders(context.Context) ([]domain.OIDCProvider, error) {
	return nil, errors.New("secret database outage")
}

type failingProvisionRoleStore struct{ store.Store }

func (failingProvisionRoleStore) SaveRole(context.Context, store.RoleSave) error {
	return errors.New("secret database outage")
}

func TestProvisionEndpointRedactsInfrastructureFailures(t *testing.T) {
	const body = `{"apiVersion":"suxen.io/v1","resources":[{"kind":"role","name":"reader","spec":{"privileges":[]}}]}`
	for _, trial := range []struct {
		name       string
		wrap       func(store.Store) store.Store
		wantStatus int
		wantCode   string
	}{
		{"top-level", func(base store.Store) store.Store { return failingProvisionValidationStore{base} }, http.StatusServiceUnavailable, "provisioning_unavailable"},
		{"resource", func(base store.Store) store.Store { return failingProvisionRoleStore{base} }, http.StatusOK, ""},
	} {
		t.Run(trial.name, func(t *testing.T) {
			fixture := newServerFixture(t)
			fixture.Handler.metadata = trial.wrap(fixture.Metadata)
			var logs bytes.Buffer
			fixture.Handler.log = slog.New(slog.NewTextHandler(&logs, nil))
			request := httptest.NewRequest(http.MethodPost, "/api/v1/provision", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+testToken)
			request.Header.Set("X-Request-ID", "provision-test-123")
			recorder := httptest.NewRecorder()
			fixture.Handler.ServeHTTP(recorder, request)
			if recorder.Code != trial.wantStatus {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "secret database outage") {
				t.Fatalf("response leaked infrastructure error: %s", recorder.Body.String())
			}
			if trial.wantCode != "" {
				var problem struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil || problem.Code != trial.wantCode {
					t.Fatalf("problem=%+v err=%v", problem, err)
				}
			} else {
				var report provision.Report
				if err := json.Unmarshal(recorder.Body.Bytes(), &report); err != nil || !report.Failed() || report.Results[0].Error != "internal provisioning error" {
					t.Fatalf("report=%+v err=%v", report, err)
				}
			}
			if !strings.Contains(logs.String(), "secret database outage") || !strings.Contains(logs.String(), "provision-test-123") {
				t.Fatalf("uncorrelated log: %s", logs.String())
			}
		})
	}
}

func TestProvisionEndpointKeepsActionableValidationDetail(t *testing.T) {
	fixture := newServerFixture(t)
	body := []byte(`{"apiVersion":"suxen.io/v1","resources":[{"kind":"user","name":"automation","spec":{"password":"secret","roles":["missing-role"]}}]}`)
	response := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/provision", body, "application/json", true)
	defer response.Body.Close()
	assertStatus(t, response, http.StatusBadRequest)
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "invalid_provisioning_document" || !strings.Contains(problem.Detail, "missing-role") {
		t.Fatalf("problem = %+v", problem)
	}
}

func TestProvisionEndpointAppliesDesiredState(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	body := []byte(`
apiVersion: suxen.io/v1
resources:
  - kind: role
    name: release-reader
    spec:
      privileges: [repository:raw:read]
`)
	response := fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision?dryRun=false&prune=false",
		body,
		"application/yaml",
		true,
	)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()
	var report provision.Report
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	created := false
	for _, result := range report.Results {
		if result.Kind == "role" && result.Name == "release-reader" {
			created = result.Status == provision.StatusCreated
		}
	}
	if !created {
		t.Fatalf("report = %+v, want created release-reader role", report)
	}
	if _, err := fixture.Metadata.Role(context.Background(), "release-reader"); err != nil {
		t.Fatalf("provisioned role was not persisted: %v", err)
	}
}

func TestProvisionOwnershipRequiresExplicitTransfer(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	body := []byte(`{
  "apiVersion": "suxen.io/v1",
  "resources": [{
    "kind": "role",
    "name": "managed-reader",
    "spec": {"description": "managed", "privileges": ["repository:raw:read"]}
  }]
}`)
	applied := fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision",
		body,
		"application/json",
		true,
	)
	assertStatus(t, applied, http.StatusOK)
	applied.Body.Close()

	listed := fixture.request(t, http.MethodGet, "/api/v1/provision", nil, true)
	assertStatus(t, listed, http.StatusOK)
	var page struct {
		Items []managedResourceRecord `json:"items"`
	}
	if err := json.NewDecoder(listed.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	listed.Body.Close()
	found := false
	for _, resource := range page.Items {
		if resource.Kind == "role" && resource.Name == "managed-reader" {
			found = true
		}
	}
	if !found {
		t.Fatalf("managed resources = %+v, want managed-reader role", page.Items)
	}

	read := fixture.request(t, http.MethodGet, "/api/v1/roles/managed-reader", nil, true)
	assertStatus(t, read, http.StatusOK)
	var role domain.Role
	if err := json.NewDecoder(read.Body).Decode(&role); err != nil {
		t.Fatal(err)
	}
	read.Body.Close()
	if !role.Managed {
		t.Fatal("provisioned role is not marked managed")
	}

	updateBody := []byte(`{
  "description": "imperative",
  "privileges": ["repository:raw:read"],
  "includedRoles": []
}`)
	blocked := fixture.request(
		t,
		http.MethodPut,
		"/api/v1/roles/managed-reader",
		updateBody,
		true,
	)
	assertStatus(t, blocked, http.StatusConflict)
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(blocked.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	blocked.Body.Close()
	if problem.Code != "managed_resource" {
		t.Fatalf("problem code = %q, want managed_resource", problem.Code)
	}

	forced := fixture.request(
		t,
		http.MethodPut,
		"/api/v1/roles/managed-reader?force=true",
		updateBody,
		true,
	)
	assertStatus(t, forced, http.StatusOK)
	if err := json.NewDecoder(forced.Body).Decode(&role); err != nil {
		t.Fatal(err)
	}
	forced.Body.Close()
	if role.Managed || role.Description != "imperative" {
		t.Fatalf("forced role response = %+v", role)
	}
	if _, err := fixture.Metadata.ProvisionRecord(
		context.Background(), "role", "managed-reader",
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("provision ownership remains after force: %v", err)
	}
}

func TestManagedUserOwnershipTransfersOnlyWithForce(t *testing.T) {
	fixture := newServerFixture(t)
	body := []byte(`{
  "apiVersion": "suxen.io/v1",
  "resources": [{
    "kind": "user",
    "name": "managed-bot",
    "spec": {"password": "declarative-secret", "admin": false, "roles": []}
  }]
}`)
	applied := fixture.requestWithContentType(
		t, http.MethodPost, "/api/v1/provision", body, "application/json", true,
	)
	assertStatus(t, applied, http.StatusOK)
	applied.Body.Close()

	update := []byte(`{"admin": true, "roles": []}`)

	blocked := fixture.request(t, http.MethodPut, "/api/v1/users/managed-bot", update, true)
	assertStatus(t, blocked, http.StatusConflict)
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(blocked.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	blocked.Body.Close()
	if problem.Code != "managed_resource" {
		t.Fatalf("problem code = %q, want managed_resource", problem.Code)
	}

	blockedDelete := fixture.request(t, http.MethodDelete, "/api/v1/users/managed-bot", nil, true)
	assertStatus(t, blockedDelete, http.StatusConflict)
	blockedDelete.Body.Close()
	if _, err := fixture.Metadata.User(context.Background(), "managed-bot"); err != nil {
		t.Fatalf("blocked delete removed the managed user: %v", err)
	}

	forced := fixture.request(t, http.MethodPut, "/api/v1/users/managed-bot?force=true", update, true)
	assertStatus(t, forced, http.StatusOK)
	var user userResponse
	if err := json.NewDecoder(forced.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	forced.Body.Close()
	if user.Managed || !user.Admin {
		t.Fatalf("forced user response = %+v", user)
	}
	if _, err := fixture.Metadata.ProvisionRecord(
		context.Background(), "user", "managed-bot",
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("provision ownership remains after force: %v", err)
	}
}

func TestProvisionEndpointRequiresProvisionPrivilege(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	response := fixture.request(
		t,
		http.MethodPost,
		"/api/v1/provision",
		[]byte("resources: []\n"),
		false,
	)
	assertStatus(t, response, http.StatusUnauthorized)
	response.Body.Close()
}

func TestProvisionEndpointRequiresCanonicalEnvelopeAndMediaType(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	listBody := []byte("- kind: role\n  name: reader\n  spec: {}\n")
	response := fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision",
		listBody,
		"application/yaml",
		true,
	)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()

	response = fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision",
		[]byte("resources: []\n"),
		"application/octet-stream",
		true,
	)
	assertStatus(t, response, http.StatusUnsupportedMediaType)
	response.Body.Close()

	response = fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision",
		[]byte("resources: []\n"),
		"application/json",
		true,
	)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
}

func TestProvisionEndpointPreflightsBlobStoreDriverBeforeMutation(t *testing.T) {
	fixture := newServerFixture(t)
	t.Setenv("UNAVAILABLE_BLOB_CONFIGURATION", "unavailable://archive")
	body := []byte(`
resources:
  - kind: blobStore
    name: archive
    spec:
      driver: unavailable
      configurationRef:
        env: UNAVAILABLE_BLOB_CONFIGURATION
  - kind: role
    name: must-not-be-created
    spec:
      privileges: [repository:*:read]
`)
	response := fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision",
		body,
		"application/yaml",
		true,
	)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()
	var report provision.Report
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if !report.Failed() {
		t.Fatalf("report = %+v, want failed driver preflight", report)
	}
	if _, err := fixture.Metadata.Role(context.Background(), "must-not-be-created"); err == nil {
		t.Fatal("role was created before blob-store driver preflight completed")
	}
}

func TestProvisionEndpointRejectsDuplicateDesiredBlobIdentity(t *testing.T) {
	fixture := newServerFixture(t)
	fixture.Handler.blobStores.Factory = func(driver string, configuration string) (blob.Store, error) {
		return blob.NewFS(strings.TrimPrefix(configuration, "fs://"))
	}
	configuration := "fs://" + filepath.Join(t.TempDir(), "shared-blobs")
	t.Setenv("FIRST_BLOB_CONFIGURATION", configuration)
	t.Setenv("SECOND_BLOB_CONFIGURATION", configuration)
	body := []byte(`
resources:
  - kind: blobStore
    name: first
    spec:
      driver: fs
      configurationRef:
        env: FIRST_BLOB_CONFIGURATION
  - kind: blobStore
    name: second
    spec:
      driver: fs
      configurationRef:
        env: SECOND_BLOB_CONFIGURATION
`)
	response := fixture.requestWithContentType(
		t,
		http.MethodPost,
		"/api/v1/provision",
		body,
		"application/yaml",
		true,
	)
	assertStatus(t, response, http.StatusOK)
	defer response.Body.Close()
	var report provision.Report
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if !report.Failed() {
		t.Fatalf("report = %+v, want physical identity conflict", report)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := fixture.Metadata.BlobStore(context.Background(), name); err == nil {
			t.Fatalf("blob store %q was created despite duplicate preflight", name)
		}
	}
}

func TestApplyProvisionSourceWaitsForCurrentLeaseHolder(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	now := time.Now().UTC()
	acquired, err := fixture.Metadata.AcquireLease(
		context.Background(),
		provisionLeaseName,
		"other-replica",
		now,
		now.Add(500*time.Millisecond),
	)
	if err != nil || !acquired {
		t.Fatalf("seed lease: acquired=%v error=%v", acquired, err)
	}
	documentPath := filepath.Join(t.TempDir(), "desired-state.yaml")
	if err := os.WriteFile(documentPath, []byte("resources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	report, applied, err := fixture.Handler.ApplyProvisionSource(
		context.Background(),
		"file://"+documentPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	if applied || report.Failed() {
		t.Fatalf("applied=%v report=%+v", applied, report)
	}
	if elapsed := time.Since(started); elapsed < 350*time.Millisecond {
		t.Fatalf("follower returned after %v before the active lease expired", elapsed)
	}
}

func TestApplyProvisionSourceStopsForInvalidDocument(t *testing.T) {
	fixture := newServerFixture(t)
	documentPath := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(documentPath, []byte("apiVersion: unsupported/v1\nresources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := fixture.Handler.ApplyProvisionSource(context.Background(), "file://"+documentPath)
	if err == nil || startup.ShouldRetry(err) {
		t.Fatalf("invalid document error = %v, want permanent failure", err)
	}
}

func TestApplyProvisionSourceStopsForInvalidBlobStoreConfiguration(t *testing.T) {
	fixture := newServerFixture(t)
	t.Setenv("SUXEN_TEST_BAD_BLOB_STORE", "invalid-driver-configuration")
	documentPath := filepath.Join(t.TempDir(), "invalid-store.yaml")
	document := "resources:\n  - kind: blobStore\n    name: unusable\n    spec:\n      driver: missing-driver\n      configurationRef:\n        env: SUXEN_TEST_BAD_BLOB_STORE\n"
	if err := os.WriteFile(documentPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := fixture.Handler.ApplyProvisionSource(context.Background(), "file://"+documentPath)
	if err == nil || startup.ShouldRetry(err) {
		t.Fatalf("invalid blob-store error = %v, want permanent failure", err)
	}
}

func TestApplyProvisionSourceRetriesMetadataFailure(t *testing.T) {
	fixture := newServerFixture(t)
	fixture.Handler.metadata = failingProvisionRoleStore{fixture.Metadata}
	documentPath := filepath.Join(t.TempDir(), "desired.yaml")
	if err := os.WriteFile(documentPath, []byte("resources:\n  - kind: role\n    name: startup-reader\n    spec:\n      privileges: [repository:raw:read]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := fixture.Handler.ApplyProvisionSource(context.Background(), "file://"+documentPath)
	if err == nil || !startup.ShouldRetry(err) {
		t.Fatalf("metadata error = %v, want retryable failure", err)
	}
}

func TestApplyProvisionSourceAdoptsUnchangedImperativeResource(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	if err := fixture.Metadata.CreateRole(ctx, domain.Role{Name: "adopted-reader", Privileges: []string{"repository:raw:read"}}); err != nil {
		t.Fatal(err)
	}
	documentPath := filepath.Join(t.TempDir(), "desired-state.yaml")
	document := "resources:\n  - kind: role\n    name: adopted-reader\n    spec:\n      privileges: [repository:raw:read]\n"
	if err := os.WriteFile(documentPath, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	source := "file://" + documentPath
	first, applied, err := fixture.Handler.ApplyProvisionSource(ctx, source)
	if err != nil || !applied || first.Failed() {
		t.Fatalf("first startup: report=%+v applied=%v err=%v", first, applied, err)
	}
	if _, err := fixture.Metadata.ProvisionRecord(ctx, "role", "adopted-reader"); err != nil {
		t.Fatalf("unchanged role was not adopted: %v", err)
	}
	read := fixture.request(t, http.MethodGet, "/api/v1/roles/adopted-reader", nil, true)
	assertStatus(t, read, http.StatusOK)
	var role domain.Role
	if err := json.NewDecoder(read.Body).Decode(&role); err != nil {
		t.Fatal(err)
	}
	read.Body.Close()
	if !role.Managed {
		t.Fatalf("adopted role=%+v", role)
	}
	second, applied, err := fixture.Handler.ApplyProvisionSource(ctx, source)
	if err != nil || applied || second.Failed() {
		t.Fatalf("second startup: report=%+v applied=%v err=%v", second, applied, err)
	}
	blocked := fixture.request(t, http.MethodPut, "/api/v1/roles/adopted-reader",
		[]byte(`{"privileges":["repository:raw:read"],"description":"imperative"}`), true)
	assertStatus(t, blocked, http.StatusConflict)
	blocked.Body.Close()
	pruned := fixture.requestWithContentType(t, http.MethodPost, "/api/v1/provision?prune=true",
		[]byte(`{"apiVersion":"suxen.io/v1","resources":[]}`), "application/json", true)
	assertStatus(t, pruned, http.StatusOK)
	pruned.Body.Close()
	if _, err := fixture.Metadata.Role(ctx, "adopted-reader"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("adopted role survived prune: %v", err)
	}
}

func TestProvisionLeaseRenewsBeyondItsInitialDuration(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialDuration := 120 * time.Millisecond
	now := time.Now().UTC()
	acquired, err := fixture.Metadata.AcquireLease(
		ctx,
		provisionLeaseName,
		fixture.Handler.schedulerID,
		now,
		now.Add(initialDuration),
	)
	if err != nil || !acquired {
		t.Fatalf("seed provisioning lease: acquired=%v error=%v", acquired, err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	applyContext, cancelApply := context.WithCancel(ctx)
	go fixture.Handler.renewProvisionLeaseWithTiming(
		applyContext,
		cancelApply,
		stop,
		done,
		30*time.Millisecond,
		initialDuration,
	)
	time.Sleep(2 * initialDuration)

	contenderNow := time.Now().UTC()
	contenderAcquired, err := fixture.Metadata.AcquireLease(
		ctx,
		provisionLeaseName,
		"other-replica",
		contenderNow,
		contenderNow.Add(initialDuration),
	)
	if err != nil {
		t.Fatal(err)
	}
	if contenderAcquired {
		t.Fatal("another replica acquired a lease that should have been renewed")
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("renew provisioning lease: %v", err)
	}
}

func TestProvisionLeaseLossCancelsReconciliation(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Seed an already expired lease and let the contender replace it before
	// starting renewal. Racing short sleeps against the renewal ticker lets a
	// busy runner renew the original lease before the contender gets scheduled.
	now := time.Now().UTC().Add(-time.Minute)
	acquired, err := fixture.Metadata.AcquireLease(
		ctx,
		provisionLeaseName,
		fixture.Handler.schedulerID,
		now,
		now.Add(50*time.Millisecond),
	)
	if err != nil || !acquired {
		t.Fatalf("seed provisioning lease: acquired=%v error=%v", acquired, err)
	}
	contenderNow := time.Now().UTC()
	contenderAcquired, err := fixture.Metadata.AcquireLease(
		ctx,
		provisionLeaseName,
		"other-replica",
		contenderNow,
		contenderNow.Add(time.Minute),
	)
	if err != nil || !contenderAcquired {
		t.Fatalf("replace expired lease: acquired=%v error=%v", contenderAcquired, err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	applyContext, cancelApply := context.WithCancel(ctx)
	defer cancelApply()
	go fixture.Handler.renewProvisionLeaseWithTiming(
		applyContext,
		cancelApply,
		stop,
		done,
		10*time.Millisecond,
		time.Minute,
	)
	select {
	case renewalErr := <-done:
		if renewalErr == nil || !strings.Contains(renewalErr.Error(), "lost") {
			t.Fatalf("renewal error = %v, want lost lease", renewalErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease renewal did not observe lost ownership")
	}
	select {
	case <-applyContext.Done():
	default:
		t.Fatal("lost lease did not cancel reconciliation context")
	}
}

func TestLoadProvisionDirectoryEnforcesAggregateLimit(t *testing.T) {
	directory := t.TempDir()
	contents := "resources: []\n#" + strings.Repeat("x", provision.MaxDocumentSize/2)
	for _, name := range []string{"one.yaml", "two.yaml"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := loadProvisionSource("file://" + directory)
	if err == nil || !strings.Contains(err.Error(), "directory exceeds") {
		t.Fatalf("error = %v, want aggregate size rejection", err)
	}
}

func TestBuiltInProvisioningToleratesConcurrentCreators(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []string{"raw", "oci"} {
		if err := fixture.Metadata.DeleteRepository(ctx, repository, store.Ownership{Force: true}); err != nil {
			t.Fatal(err)
		}
		if err := fixture.Metadata.DeleteProvisionRecord(ctx, "repository", repository); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"anonymous", "administrator"} {
		if err := fixture.Metadata.DeleteRole(ctx, role, store.Ownership{Force: true}); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	errorsByReplica := make([]error, 2)
	var wait sync.WaitGroup
	for replica := range errorsByReplica {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			report, err := fixture.Handler.provisionEngine().Apply(
				ctx,
				provision.Document{APIVersion: provision.APIVersion},
				provision.Options{},
			)
			if err == nil && report.Failed() {
				err = fmt.Errorf("failed report: %+v", report.Results)
			}
			errorsByReplica[index] = err
		}(replica)
	}
	close(start)
	wait.Wait()
	for replica, err := range errorsByReplica {
		if err != nil {
			t.Fatalf("replica %d: %v", replica, err)
		}
	}
}
