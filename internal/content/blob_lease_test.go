package content

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/store"
)

func TestBlobStoreLeaseExcludesAnotherRuntime(t *testing.T) {
	metadata, err := store.OpenSQLite(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	if err := metadata.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := New(Options{Metadata: metadata, SchedulerID: "node-a"})
	second := New(Options{Metadata: metadata, SchedulerID: "node-b"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.WithBlobStoreLease(ctx, "shared", func(context.Context) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.WithBlobStoreLease(ctx, "shared", func(context.Context) error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second runtime entered while the first still held the store lease")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}
