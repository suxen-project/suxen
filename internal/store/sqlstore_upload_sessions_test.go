package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestUploadSessionCleanupIgnoresReducedQuotaButKeepsExclusiveLease(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := createUploadTestRepository(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	repository, err := metadata.Repository(ctx, "registry")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := testUploadSession("quota-cleanup", "alice", now)
	session.RepositoryID = repository.ID
	initial := UploadSessionLimits{MaxStagedBytes: 10, MaxPrincipalStagedBytes: 10, MaxPrincipalSessions: 4}
	if err := metadata.CreateUploadSession(ctx, session, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.ReserveUploadSession(ctx, session.UploadSessionIdentity,
		"append", now, now.Add(time.Minute), 8, true, 10, initial); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CommitUploadSessionAppend(ctx, session.ID, "append", 8, now, false); err != nil {
		t.Fatal(err)
	}
	reduced := UploadSessionLimits{MaxStagedBytes: 4, MaxPrincipalStagedBytes: 4, MaxPrincipalSessions: 4}
	if _, err := metadata.ReserveUploadSession(ctx, session.UploadSessionIdentity,
		"finalize-quota", now, now.Add(time.Minute), 0, true, 10, reduced); !errors.Is(err, domain.ErrUploadSessionQuotaExceeded) {
		t.Fatalf("finalization with reduced aggregate quota = %v; want quota exceeded", err)
	}
	if _, err := metadata.ReserveUploadSession(ctx, session.UploadSessionIdentity,
		"finalize", now, now.Add(time.Minute), 0, true, 4, reduced); !errors.Is(err, domain.ErrUploadSessionSizeExceeded) {
		t.Fatalf("finalization with reduced size limit = %v; want size exceeded", err)
	}
	wrong := session.UploadSessionIdentity
	wrong.Principal = "bob"
	if _, err := metadata.ReserveUploadSessionCleanup(ctx, wrong, "wrong-owner", now, now.Add(time.Minute)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cleanup under wrong identity = %v; want not found", err)
	}
	reservation, err := metadata.ReserveUploadSessionCleanup(ctx, session.UploadSessionIdentity,
		"cleanup", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ReservedGrow != 0 || reservation.Session.Size != 8 {
		t.Fatalf("cleanup reservation = %+v; want existing size and no reserved growth", reservation)
	}
	if _, err := metadata.ReserveUploadSessionCleanup(ctx, session.UploadSessionIdentity,
		"concurrent", now, now.Add(time.Minute)); !errors.Is(err, domain.ErrUploadSessionBusy) {
		t.Fatalf("concurrent cleanup = %v; want busy", err)
	}
	if err := metadata.DeleteUploadSession(ctx, session.ID, "cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestUploadSessionLeaseRenewalPreservesOwnerAndCrashExpiry(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()
	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := createUploadTestRepository(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	repository, err := metadata.Repository(ctx, "registry")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	session := testUploadSession("renewed", "alice", now)
	session.RepositoryID = repository.ID
	limits := UploadSessionLimits{MaxStagedBytes: 10, MaxPrincipalStagedBytes: 10, MaxPrincipalSessions: 4}
	if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.ReserveUploadSession(ctx, session.UploadSessionIdentity,
		"live-append", now, now.Add(100*time.Millisecond), 3, true, 10, limits); err != nil {
		t.Fatal(err)
	}
	if err := metadata.RenewUploadSessionOperation(ctx, session.ID, "other-owner",
		now.Add(70*time.Millisecond), now.Add(170*time.Millisecond)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign renewal = %v, want not found", err)
	}
	if err := metadata.RenewUploadSessionOperation(ctx, session.ID, "live-append",
		now.Add(70*time.Millisecond), now.Add(170*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.ReserveUploadSessionCleanup(ctx, session.UploadSessionIdentity,
		"competing-cleanup", now.Add(120*time.Millisecond), now.Add(time.Second)); !errors.Is(err, domain.ErrUploadSessionBusy) {
		t.Fatalf("cleanup after original expiry = %v, want busy", err)
	}
	// Once the process stops renewing, the finite lease recovers normally.
	if _, err := metadata.ReserveUploadSessionCleanup(ctx, session.UploadSessionIdentity,
		"recovery-cleanup", now.Add(180*time.Millisecond), now.Add(time.Second)); err != nil {
		t.Fatalf("cleanup after renewed expiry: %v", err)
	}
}

func TestSQLiteUploadSessionsEnforceOwnershipAndQuotas(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := createUploadTestRepository(ctx, metadata); err != nil {
		t.Fatal(err)
	}

	limits := UploadSessionLimits{
		MaxStagedBytes:          10,
		MaxPrincipalStagedBytes: 6,
		MaxPrincipalSessions:    2,
	}
	now := time.Now().UTC()
	repository, err := metadata.Repository(ctx, "registry")
	if err != nil {
		t.Fatal(err)
	}
	first := testUploadSession("first", "alice", now)
	second := testUploadSession("second", "alice", now)
	third := testUploadSession("third", "alice", now)
	bob := testUploadSession("bob", "bob", now)
	first.RepositoryID, second.RepositoryID, third.RepositoryID, bob.RepositoryID = repository.ID, repository.ID, repository.ID, repository.ID
	for _, session := range []UploadSession{first, second} {
		if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
			t.Fatalf("create %s: %v", session.ID, err)
		}
	}
	if err := metadata.CreateUploadSession(ctx, third, limits); !errors.Is(
		err,
		domain.ErrUploadSessionQuotaExceeded,
	) {
		t.Fatalf("third principal session returned %v, want quota error", err)
	}
	if err := metadata.CreateUploadSession(ctx, bob, limits); err != nil {
		t.Fatalf("create session for independent principal: %v", err)
	}

	wrongIdentity := first.UploadSessionIdentity
	wrongIdentity.Image = "another-image"
	if _, err := metadata.ReserveUploadSession(
		ctx,
		wrongIdentity,
		"wrong-owner",
		now,
		now.Add(time.Minute),
		1,
		true,
		8,
		limits,
	); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("wrong identity returned %v, want ErrNotFound", err)
	}

	reservation, err := metadata.ReserveUploadSession(
		ctx,
		first.UploadSessionIdentity,
		"append-first",
		now,
		now.Add(time.Minute),
		4,
		true,
		8,
		limits,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ReservedGrow != 4 || reservation.MaxSize != 4 {
		t.Fatalf("unexpected reservation: %+v", reservation)
	}
	if _, err := metadata.ReserveUploadSession(
		ctx,
		first.UploadSessionIdentity,
		"concurrent-append",
		now,
		now.Add(time.Minute),
		1,
		true,
		8,
		limits,
	); !errors.Is(err, domain.ErrUploadSessionBusy) {
		t.Fatalf("concurrent operation returned %v, want busy error", err)
	}
	if err := metadata.CommitUploadSessionAppend(
		ctx,
		first.ID,
		"append-first",
		4,
		now.Add(time.Second),
		false,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := metadata.ReserveUploadSession(
		ctx,
		second.UploadSessionIdentity,
		"principal-over-budget",
		now,
		now.Add(time.Minute),
		3,
		true,
		8,
		limits,
	); !errors.Is(err, domain.ErrUploadSessionQuotaExceeded) {
		t.Fatalf("principal over-budget reservation returned %v, want quota error", err)
	}
	if _, err := metadata.ReserveUploadSession(
		ctx,
		bob.UploadSessionIdentity,
		"store-over-budget",
		now,
		now.Add(time.Minute),
		7,
		true,
		8,
		limits,
	); !errors.Is(err, domain.ErrUploadSessionQuotaExceeded) {
		t.Fatalf("store over-budget reservation returned %v, want quota error", err)
	}

	unknownLength, err := metadata.ReserveUploadSession(
		ctx,
		bob.UploadSessionIdentity,
		"unknown-length",
		now,
		now.Add(time.Minute),
		8,
		false,
		8,
		limits,
	)
	if err != nil {
		t.Fatal(err)
	}
	if unknownLength.ReservedGrow != 6 || unknownLength.MaxSize != 6 {
		t.Fatalf("unknown-length reservation was not capped by store capacity: %+v", unknownLength)
	}
	if err := metadata.ReleaseUploadSessionOperation(ctx, bob.ID, "unknown-length"); err != nil {
		t.Fatal(err)
	}
}

func TestUploadSessionReservationRecoversExpiredLeaseOnBothDialects(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			metadata := openCompanionTestStore(t, backend)
			ctx := context.Background()
			if err := createUploadTestRepository(ctx, metadata); err != nil {
				t.Fatal(err)
			}
			limits := UploadSessionLimits{
				MaxStagedBytes:          10,
				MaxPrincipalStagedBytes: 10,
				MaxPrincipalSessions:    2,
			}
			now := time.Now().UTC()
			session := testUploadSession("recover", "alice", now)
			repository, err := metadata.Repository(ctx, "registry")
			if err != nil {
				t.Fatal(err)
			}
			session.RepositoryID = repository.ID
			if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ReserveUploadSession(
				ctx,
				session.UploadSessionIdentity,
				"expired",
				now,
				now.Add(time.Second),
				5,
				true,
				10,
				limits,
			); err != nil {
				t.Fatal(err)
			}
			bob := testUploadSession("other", "bob", now)
			bob.RepositoryID = repository.ID
			if err := metadata.CreateUploadSession(ctx, bob, limits); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ReserveUploadSession(
				ctx, bob.UploadSessionIdentity, "cannot-spend-uncertain-bytes",
				now.Add(2*time.Second), now.Add(time.Minute), 6, true, 10, limits,
			); !errors.Is(err, domain.ErrUploadSessionQuotaExceeded) {
				t.Fatalf("expired operation's capacity was reusable before recovery: %v", err)
			}
			if _, err := metadata.ReserveUploadSession(
				ctx,
				session.UploadSessionIdentity,
				"replacement",
				now.Add(2*time.Second),
				now.Add(time.Minute),
				4,
				true,
				10,
				limits,
			); !errors.Is(err, domain.ErrUploadSessionBusy) {
				t.Fatalf("growth without physical reconciliation = %v, want busy", err)
			}
			recovery, err := metadata.ReserveUploadSessionCleanup(
				ctx, session.UploadSessionIdentity, "recovery",
				now.Add(2*time.Second), now.Add(time.Minute),
			)
			if err != nil {
				t.Fatal(err)
			}
			if recovery.Session.ReservedBytes != 5 {
				t.Fatalf("cleanup reservation discarded uncertain capacity: %+v", recovery)
			}
			// A crash before the physical append leaves no bytes to account for. Only
			// after checking the object under the recovery lease may the hold be freed.
			if err := metadata.ReconcileUploadSessionSize(
				ctx, session.UploadSessionIdentity, "recovery", 0, now.Add(2*time.Second),
			); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.ReserveUploadSession(
				ctx, bob.UploadSessionIdentity, "after-recovery",
				now.Add(2*time.Second), now.Add(time.Minute), 6, true, 10, limits,
			); err != nil {
				t.Fatalf("recovered capacity stayed reserved: %v", err)
			}
		})
	}
}

func testUploadSession(id string, principal string, now time.Time) UploadSession {
	return UploadSession{
		UploadSessionIdentity: UploadSessionIdentity{
			ID:         id,
			Repository: "registry",
			Image:      "acme/application",
			BlobStore:  "default",
			Principal:  principal,
		},
		StorageKey: "oci/test/" + id,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func TestSQLiteStaleUploadSessionsExcludeActiveAndLeasedSessions(t *testing.T) {
	metadata, err := OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer metadata.Close()

	ctx := context.Background()
	migrateTestMetadata(t, metadata)
	if err := createUploadTestRepository(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	limits := UploadSessionLimits{
		MaxStagedBytes:          10,
		MaxPrincipalStagedBytes: 10,
		MaxPrincipalSessions:    4,
	}
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	stale := testUploadSession("stale", "alice", old)
	expiredLease := testUploadSession("expired-lease", "alice", old)
	liveLease := testUploadSession("live-lease", "alice", old)
	active := testUploadSession("active", "alice", now)
	repository, err := metadata.Repository(ctx, "registry")
	if err != nil {
		t.Fatal(err)
	}
	stale.RepositoryID, expiredLease.RepositoryID, liveLease.RepositoryID, active.RepositoryID = repository.ID, repository.ID, repository.ID, repository.ID
	for _, session := range []UploadSession{stale, expiredLease, liveLease, active} {
		if err := metadata.CreateUploadSession(ctx, session, limits); err != nil {
			t.Fatalf("create %s: %v", session.ID, err)
		}
	}
	for _, lease := range []struct {
		session   UploadSession
		expiresAt time.Time
	}{
		{session: expiredLease, expiresAt: now.Add(-time.Minute)},
		{session: liveLease, expiresAt: now.Add(time.Hour)},
	} {
		if _, err := metadata.ReserveUploadSession(
			ctx,
			lease.session.UploadSessionIdentity,
			"lease-"+lease.session.ID,
			old,
			lease.expiresAt,
			1,
			true,
			10,
			limits,
		); err != nil {
			t.Fatalf("lease %s: %v", lease.session.ID, err)
		}
	}

	sessions, err := metadata.StaleUploadSessions(ctx, "default", now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, session := range sessions {
		ids = append(ids, session.ID)
	}
	if len(ids) != 2 || ids[0] != "expired-lease" && ids[1] != "expired-lease" ||
		ids[0] != "stale" && ids[1] != "stale" {
		t.Fatalf("stale sessions = %v, want expired-lease and stale", ids)
	}
	if other, err := metadata.StaleUploadSessions(ctx, "other", now, now); err != nil ||
		len(other) != 0 {
		t.Fatalf("other blob store returned %v, %v", other, err)
	}
}
