package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestProxyFetchPublicationOrderingOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		name := fmt.Sprintf("proxy-order-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: name, Format: "raw", Type: "proxy", Upstream: "https://example.org"}); err != nil {
			t.Fatal(err)
		}
		repo, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		// Separate views stand in for independent replicas sharing the same DB.
		first := forRepository(metadata, repo)
		second := forRepository(metadata, repo)
		begin := func(view RepositoryView, path string) domain.ProxyFetchToken {
			t.Helper()
			token, err := view.BeginProxyFetch(ctx, path, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			return token
		}
		put := func(path, digest string, token domain.ProxyFetchToken) error {
			t.Helper()
			_, err := metadata.PutAssets(ctx, []domain.Asset{{Repository: name, RepositoryID: repo.ID, Path: path, Digest: digest, ProxyFetch: token}})
			return err
		}

		t.Run("late 404 cannot hide newer 200", func(t *testing.T) {
			old := begin(first, "index-a")
			newer := begin(second, "index-a")
			if newer.Sequence <= old.Sequence || newer.Epoch != old.Epoch {
				t.Fatalf("unordered tokens: %+v %+v", old, newer)
			}
			if err := put("index-a", "new", newer); err != nil {
				t.Fatal(err)
			}
			published, err := first.PublishProxyNotFound(ctx, "index-a", old, time.Now().Add(time.Minute))
			if err != nil || published {
				t.Fatalf("stale 404 published=%v err=%v", published, err)
			}
			if hit, err := first.NegativeCacheHit(ctx, "index-a", time.Now()); err != nil || hit {
				t.Fatalf("negative cache hit=%v err=%v", hit, err)
			}
		})

		t.Run("late 200 cannot replace newer 404", func(t *testing.T) {
			old := begin(first, "index-b")
			newer := begin(second, "index-b")
			published, err := second.PublishProxyNotFound(ctx, "index-b", newer, time.Now().Add(time.Minute))
			if err != nil || !published {
				t.Fatalf("newer 404 published=%v err=%v", published, err)
			}
			if err := put("index-b", "old", old); !errors.Is(err, ErrProxyResultSuperseded) {
				t.Fatalf("old 200 publication=%v", err)
			}
			if hit, err := first.NegativeCacheHit(ctx, "index-b", time.Now()); err != nil || !hit {
				t.Fatalf("newer 404 lost: hit=%v err=%v", hit, err)
			}
		})

		t.Run("genuine later 404 wins", func(t *testing.T) {
			initial := begin(first, "index-c")
			if err := put("index-c", "available", initial); err != nil {
				t.Fatal(err)
			}
			later := begin(second, "index-c")
			published, err := second.PublishProxyNotFound(ctx, "index-c", later, time.Now().Add(time.Minute))
			if err != nil || !published {
				t.Fatalf("later 404 published=%v err=%v", published, err)
			}
			if hit, err := first.NegativeCacheHit(ctx, "index-c", time.Now()); err != nil || !hit {
				t.Fatalf("later 404 missing: hit=%v err=%v", hit, err)
			}
			if _, err := second.Asset(ctx, "index-c"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("superseded positive row remains: %v", err)
			}
			if hit, err := first.NegativeCacheHit(ctx, "index-c", time.Now().Add(2*time.Minute)); err != nil || hit {
				t.Fatalf("negative expiry: hit=%v err=%v", hit, err)
			}
			if _, err := second.Asset(ctx, "index-c"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("positive row resurrected after negative expiry: %v", err)
			}
		})

		t.Run("expired epoch rejects stale result", func(t *testing.T) {
			old, err := first.BeginProxyFetch(ctx, "index-d", time.Now().Add(-time.Second))
			if err != nil {
				t.Fatal(err)
			}
			newer := begin(second, "index-d")
			if newer.Epoch == old.Epoch {
				t.Fatalf("expired key reused epoch %q", old.Epoch)
			}
			if err := put("index-d", "current", newer); err != nil {
				t.Fatal(err)
			}
			if err := put("index-d", "old", old); !errors.Is(err, ErrProxyResultSuperseded) {
				t.Fatalf("expired old publication=%v", err)
			}
			asset, err := second.Asset(ctx, "index-d")
			if err != nil || asset.Digest != "current" {
				t.Fatalf("recycled key asset=%+v err=%v", asset, err)
			}
		})

		t.Run("late 304 cannot refresh replaced asset", func(t *testing.T) {
			initial := begin(first, "index-e")
			if err := put("index-e", "before", initial); err != nil {
				t.Fatal(err)
			}
			prior, err := first.Asset(ctx, "index-e")
			if err != nil {
				t.Fatal(err)
			}
			old304 := begin(first, "index-e")
			newer200 := begin(second, "index-e")
			if err := put("index-e", "after", newer200); err != nil {
				t.Fatal(err)
			}
			published, err := first.PublishProxyNotModified(ctx, "index-e", prior.ID, old304, time.Now())
			if err != nil || published {
				t.Fatalf("stale 304 published=%v err=%v", published, err)
			}
			current, err := second.Asset(ctx, "index-e")
			if err != nil || current.Digest != "after" {
				t.Fatalf("current asset=%+v err=%v", current, err)
			}
		})

		t.Run("late 304 cannot clear newer 404", func(t *testing.T) {
			initial := begin(first, "index-f")
			if err := put("index-f", "before", initial); err != nil {
				t.Fatal(err)
			}
			prior, err := first.Asset(ctx, "index-f")
			if err != nil {
				t.Fatal(err)
			}
			old304 := begin(first, "index-f")
			newer404 := begin(second, "index-f")
			published, err := second.PublishProxyNotFound(ctx, "index-f", newer404, time.Now().Add(time.Minute))
			if err != nil || !published {
				t.Fatalf("newer 404 published=%v err=%v", published, err)
			}
			published, err = first.PublishProxyNotModified(ctx, "index-f", prior.ID, old304, time.Now())
			if err != nil || published {
				t.Fatalf("stale 304 published=%v err=%v", published, err)
			}
			if hit, err := first.NegativeCacheHit(ctx, "index-f", time.Now()); err != nil || !hit {
				t.Fatalf("newer 404 lost: hit=%v err=%v", hit, err)
			}
		})

		t.Run("repository recreation does not inherit generation", func(t *testing.T) {
			stale := begin(first, "index-g")
			if err := metadata.DeleteRepository(ctx, name, Ownership{}); err != nil {
				t.Fatal(err)
			}
			if err := metadata.CreateRepository(ctx, domain.Repository{Name: name, Format: "raw", Type: "proxy", Upstream: "https://example.org"}); err != nil {
				t.Fatal(err)
			}
			freshRepo, err := metadata.Repository(ctx, name)
			if err != nil || freshRepo.ID == repo.ID {
				t.Fatalf("replacement repository=%+v err=%v", freshRepo, err)
			}
			published, err := first.PublishProxyNotFound(ctx, "index-g", stale, time.Now().Add(time.Minute))
			if err != nil || published {
				t.Fatalf("stale repository 404 published=%v err=%v", published, err)
			}
			fresh := forRepository(metadata, freshRepo)
			current := begin(fresh, "index-g")
			published, err = fresh.PublishProxyNotFound(ctx, "index-g", current, time.Now().Add(time.Minute))
			if err != nil || !published {
				t.Fatalf("fresh repository 404 published=%v err=%v", published, err)
			}
			if hit, err := fresh.NegativeCacheHit(ctx, "index-g", time.Now()); err != nil || !hit {
				t.Fatalf("replacement negative cache hit=%v err=%v", hit, err)
			}
		})
	})
}

func TestProxyFetchPrunesExpiredCacheRowsInBatchesOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		name := fmt.Sprintf("proxy-prune-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: name, Format: "raw", Type: "proxy", Upstream: "https://example.org"}); err != nil {
			t.Fatal(err)
		}
		repo, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		pastWhole := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
		for index := range 258 {
			expiry := pastWhole
			if index%2 != 0 {
				expiry = expiry.Add(500 * time.Millisecond)
			}
			path := fmt.Sprintf("old-%03d", index)
			if err := metadata.PutNegativeCacheByRepositoryID(ctx, repo.ID, path, expiry); err != nil {
				t.Fatal(err)
			}
			if _, err := metadata.db.ExecContext(ctx, `INSERT INTO proxy_cache_state
				(repository_id, path, epoch, next_sequence, published_sequence, expires_at_ns)
				VALUES (?, ?, ?, 1, 0, ?)`, repo.ID, path, "expired", pastWhole.UnixNano()); err != nil {
				t.Fatal(err)
			}
		}
		if err := metadata.PutNegativeCacheByRepositoryID(ctx, repo.ID, "live", time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		view := forRepository(metadata, repo)
		for fetch, remaining := range []int{2, 0} {
			if _, err := view.BeginProxyFetch(ctx, fmt.Sprintf("new-%d", fetch), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"negative_cache", "proxy_cache_state"} {
				var count int
				if err := metadata.db.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM `+table+` WHERE repository_id = ? AND path LIKE 'old-%'`, repo.ID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != remaining {
					t.Fatalf("%s old rows after fetch %d = %d, want %d", table, fetch, count, remaining)
				}
			}
		}
		if hit, err := view.NegativeCacheHit(ctx, "live", time.Now()); err != nil || !hit {
			t.Fatalf("live negative entry lost: hit=%v err=%v", hit, err)
		}
	})
}

func TestProxyFetchResetsExpiredRequestedKeyBehindCleanupBatchOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		name := fmt.Sprintf("proxy-epoch-reset-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: name, Format: "raw", Type: "proxy", Upstream: "https://example.org"}); err != nil {
			t.Fatal(err)
		}
		repo, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		view := forRepository(metadata, repo)
		old, err := view.BeginProxyFetch(ctx, "target", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := metadata.db.ExecContext(ctx, `UPDATE proxy_cache_state SET published_sequence = 1
			WHERE repository_id = ? AND path = ?`, repo.ID, "target"); err != nil {
			t.Fatal(err)
		}
		// The cleanup batch removes these older rows, leaving the expired
		// target to be handled by BeginProxyFetch's conflict path.
		for index := range 256 {
			path := fmt.Sprintf("older-%03d", index)
			if _, err := metadata.db.ExecContext(ctx, `INSERT INTO proxy_cache_state
				(repository_id, path, epoch, next_sequence, published_sequence, expires_at_ns)
				VALUES (?, ?, ?, 1, 0, ?)`, repo.ID, path, "older", time.Now().Add(-2*time.Minute).UnixNano()); err != nil {
				t.Fatal(err)
			}
		}
		fresh, err := view.BeginProxyFetch(ctx, "target", time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Epoch == old.Epoch || fresh.Sequence != 1 {
			t.Fatalf("expired key retained old generation: old=%+v fresh=%+v", old, fresh)
		}
		var watermark int64
		if err := metadata.db.QueryRowContext(ctx, `SELECT published_sequence FROM proxy_cache_state
			WHERE repository_id = ? AND path = ?`, repo.ID, "target").Scan(&watermark); err != nil || watermark != 0 {
			t.Fatalf("recycled key publication watermark=%d err=%v", watermark, err)
		}
		if published, err := view.PublishProxyNotFound(ctx, "target", old, time.Now().Add(time.Minute)); err != nil || published {
			t.Fatalf("expired token published=%v err=%v", published, err)
		}
		if published, err := view.PublishProxyNotFound(ctx, "target", fresh, time.Now().Add(time.Minute)); err != nil || !published {
			t.Fatalf("fresh token published=%v err=%v", published, err)
		}
	})
}

func TestConcurrentProxyFetchBeginsIssueUniqueSequencesOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, _ string) {
		ctx := context.Background()
		name := fmt.Sprintf("proxy-concurrent-begin-%d", time.Now().UnixNano())
		if err := metadata.CreateRepository(ctx, domain.Repository{Name: name, Format: "raw", Type: "proxy", Upstream: "https://example.org"}); err != nil {
			t.Fatal(err)
		}
		repo, err := metadata.Repository(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		const workers = 4
		start := make(chan struct{})
		type result struct {
			token domain.ProxyFetchToken
			err   error
		}
		results := make(chan result, workers)
		for range workers {
			go func() {
				<-start
				token, err := forRepository(metadata, repo).BeginProxyFetch(ctx, "target", time.Now().Add(time.Hour))
				results <- result{token: token, err: err}
			}()
		}
		close(start)
		var epoch string
		seen := make(map[int64]bool, workers)
		for range workers {
			value := <-results
			if value.err != nil {
				t.Fatal(value.err)
			}
			if epoch == "" {
				epoch = value.token.Epoch
			}
			if value.token.Epoch != epoch || seen[value.token.Sequence] {
				t.Fatalf("duplicate or mixed generation: %+v; seen=%v", value.token, seen)
			}
			seen[value.token.Sequence] = true
		}
		for sequence := int64(1); sequence <= workers; sequence++ {
			if !seen[sequence] {
				t.Fatalf("missing sequence %d from %v", sequence, seen)
			}
		}
	})
}
