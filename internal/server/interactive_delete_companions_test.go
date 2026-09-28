package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
)

// seedStubArtifact publishes an artifact and its companion metadata record for the
// retention-stub format, whose CompanionPaths declares pkg/<name>/<version>/meta.json.
func seedStubArtifact(t *testing.T, f *serverFixture, repository, name, version string) {
	t.Helper()
	ctx := context.Background()
	base := "pkg/" + name + "/" + version
	if _, err := f.Metadata.PutAsset(ctx, domain.Asset{
		Repository: repository, Path: base + "/artifact", Kind: "raw",
		Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Metadata.PutAsset(ctx, domain.Asset{
		Repository: repository, Path: base + "/meta.json", Kind: "metadata",
		Digest: "sha256:" + strings.Repeat("b", 64), Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestInteractiveDeleteCascadesCompanions covers the repository-API artifact
// DELETE: it must remove the artifact's declared companion metadata in the same
// step, the way policy cleanup already does, so the companion is not orphaned and
// its blob does not leak. A format that declares an invalid companion path fails
// closed — the artifact is kept rather than deleted with a wrong companion.
func TestInteractiveDeleteCascadesCompanions(t *testing.T) {
	t.Parallel()
	f := newServerFixture(t)
	ctx := context.Background()
	if err := f.Metadata.CreateRepository(ctx, domain.Repository{
		Name: "delraw", Format: "retention-stub", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	seedStubArtifact(t, f, "delraw", "alpha", "1.0.0")
	seedStubArtifact(t, f, "delraw", "bad", "1.0.0")

	deleted := f.request(t, http.MethodDelete, "/repository/delraw/pkg/alpha/1.0.0/artifact", nil, true)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
	if _, err := f.Metadata.Asset(ctx, "delraw", "pkg/alpha/1.0.0/artifact"); !isNotFound(err) {
		t.Fatalf("artifact survived delete: %v", err)
	}
	if _, err := f.Metadata.Asset(ctx, "delraw", "pkg/alpha/1.0.0/meta.json"); !isNotFound(err) {
		t.Fatalf("companion metadata orphaned by interactive delete: %v", err)
	}

	// The "bad" package declares a traversing companion path; the delete fails
	// closed and keeps the artifact.
	failed := f.request(t, http.MethodDelete, "/repository/delraw/pkg/bad/1.0.0/artifact", nil, true)
	assertStatus(t, failed, http.StatusInternalServerError)
	failed.Body.Close()
	if _, err := f.Metadata.Asset(ctx, "delraw", "pkg/bad/1.0.0/artifact"); err != nil {
		t.Fatalf("artifact deleted despite invalid companion declaration: %v", err)
	}
}

// TestAdminAssetDeleteCascadesCompanions covers the same cascade on the admin
// asset DELETE endpoint, the other interactive deletion path.
func TestAdminAssetDeleteCascadesCompanions(t *testing.T) {
	t.Parallel()
	f := newServerFixture(t)
	ctx := context.Background()
	if err := f.Metadata.CreateRepository(ctx, domain.Repository{
		Name: "deladmin", Format: "retention-stub", Type: "hosted",
	}); err != nil {
		t.Fatal(err)
	}
	seedStubArtifact(t, f, "deladmin", "alpha", "1.0.0")

	artifact, err := f.Metadata.Asset(ctx, "deladmin", "pkg/alpha/1.0.0/artifact")
	if err != nil {
		t.Fatal(err)
	}
	deleted := f.request(t, http.MethodDelete,
		fmt.Sprintf("/api/v1/repositories/deladmin/assets/%d", artifact.ID), nil, true)
	assertStatus(t, deleted, http.StatusNoContent)
	deleted.Body.Close()
	if _, err := f.Metadata.Asset(ctx, "deladmin", "pkg/alpha/1.0.0/meta.json"); !isNotFound(err) {
		t.Fatalf("companion metadata orphaned by admin asset delete: %v", err)
	}
}
