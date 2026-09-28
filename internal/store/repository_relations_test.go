package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestRepositoryRelationsOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		leaf := "leaf-" + suffix
		group := "group-" + suffix
		other := "other-" + suffix
		for _, repo := range []domain.Repository{
			{Name: leaf, Format: "oci", Type: "hosted"},
			{Name: other, Format: "raw", Type: "hosted"},
		} {
			if err := metadata.CreateRepository(ctx, repo); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			members []string
			want    error
		}{
			{[]string{"absent-" + suffix}, domain.ErrInvalidRepository},
			{[]string{other}, domain.ErrInvalidRepository},
			{[]string{leaf}, nil},
		} {
			err := metadata.CreateRepository(ctx, domain.Repository{Name: group, Format: "oci", Type: "group", Members: tc.members})
			if !errors.Is(err, tc.want) {
				t.Fatalf("create members %v: %v, want %v", tc.members, err, tc.want)
			}
		}
		if err := metadata.CreateRepository(ctx, domain.Repository{
			Name: "outer-" + suffix, Format: "oci", Type: "group", Members: []string{group},
		}); !errors.Is(err, domain.ErrNestedGroupMember) {
			t.Fatalf("create nested group: %v", err)
		}
		if err := metadata.DeleteRepository(ctx, leaf, Ownership{}); !errors.Is(err, domain.ErrRepositoryInUseByGroup) {
			t.Fatalf("delete referenced leaf: %v", err)
		}
		updated := domain.Repository{Name: group, Format: "oci", Type: "group", Members: []string{other}}
		if err := metadata.UpdateRepository(ctx, updated); !errors.Is(err, domain.ErrInvalidRepository) {
			t.Fatalf("update with incompatible member: %v", err)
		}
		updated.Members = []string{group}
		if err := metadata.UpdateRepository(ctx, updated); !errors.Is(err, domain.ErrNestedGroupMember) {
			t.Fatalf("update with nested member: %v", err)
		}
		stored, err := metadata.Repository(ctx, group)
		if err != nil || len(stored.Members) != 1 || stored.Members[0] != leaf {
			t.Fatalf("rejected update changed group: %+v, %v", stored, err)
		}
	})
}

func TestRepositoryUploadDeletionOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx := context.Background()
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		upload := "upload-" + suffix
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: upload, Format: "oci", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		repository, err := metadata.Repository(ctx, upload)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		session := UploadSession{UploadSessionIdentity: UploadSessionIdentity{
			ID: "session-" + suffix, Repository: upload, Image: "acme/widget", BlobStore: "default", Principal: "alice",
		}, RepositoryID: repository.ID, StorageKey: "oci/test/" + suffix, CreatedAt: now, UpdatedAt: now}
		limits := UploadSessionLimits{MaxStagedBytes: 100, MaxPrincipalStagedBytes: 100, MaxPrincipalSessions: 2}
		if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
			t.Fatal(err)
		}
		if err := metadata.DeleteRepository(ctx, upload, Ownership{}); !errors.Is(err, domain.ErrRepositoryHasUploadSessions) {
			t.Fatalf("delete with session: %v", err)
		}
		if _, err := metadata.UploadSession(ctx, session.ID); err != nil {
			t.Fatalf("lost cleanup ledger: %v", err)
		}
		if err := metadata.DeleteUploadSession(ctx, session.ID, ""); err != nil {
			t.Fatal(err)
		}
		if err := metadata.DeleteRepository(ctx, upload, Ownership{}); err != nil {
			t.Fatalf("delete after cancellation: %v", err)
		}
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: upload, Format: "oci", Type: "hosted"}); err != nil {
			t.Fatal(err)
		}
		if err := metadata.CreateUploadSession(ctx, session, limits); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("stale upload start: %v", err)
		}
	})
}
