package provision

import (
	"context"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/store"
)

func TestPruneKeepsRepositoryWithUploadSession(t *testing.T) {
	ctx := context.Background()
	metadata := newProvisionTestStore(t)
	engine := Engine{Store: metadata}
	document := mustResolveDocument(t, `
resources:
  - kind: repository
    name: staged
    spec:
      format: oci
      type: hosted
`)
	if report, err := engine.Apply(ctx, document, Options{}); err != nil || report.Failed() {
		t.Fatalf("create provisioned repository: %+v, %v", report, err)
	}
	repository, err := metadata.Repository(ctx, "staged")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := store.UploadSession{UploadSessionIdentity: store.UploadSessionIdentity{
		ID: "staged-session", Repository: repository.Name, Image: "acme/widget", BlobStore: "default", Principal: "alice",
	}, RepositoryID: repository.ID, StorageKey: "oci/test/staged-session", CreatedAt: now, UpdatedAt: now}
	limits := store.UploadSessionLimits{MaxStagedBytes: 100, MaxPrincipalStagedBytes: 100, MaxPrincipalSessions: 2}
	if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
		t.Fatal(err)
	}
	empty := Document{APIVersion: APIVersion, Resources: []Resource{}}
	report, err := engine.Apply(ctx, empty, Options{Prune: true})
	if err == nil && !report.Failed() {
		t.Fatalf("prune removed a repository with an upload session: %+v", report)
	}
	if _, err := metadata.Repository(ctx, repository.Name); err != nil {
		t.Fatalf("prune removed repository: %v", err)
	}
	if _, err := metadata.UploadSession(ctx, session.ID); err != nil {
		t.Fatalf("prune lost upload ledger: %v", err)
	}
	if err := metadata.DeleteUploadSession(ctx, session.ID, ""); err != nil {
		t.Fatal(err)
	}
	report, err = engine.Apply(ctx, empty, Options{Prune: true})
	if err != nil || report.Failed() {
		t.Fatalf("prune after cancellation: %+v, %v", report, err)
	}
	if _, err := metadata.Repository(ctx, repository.Name); err != domain.ErrNotFound {
		t.Fatalf("repository retained after successful prune: %v", err)
	}
}
