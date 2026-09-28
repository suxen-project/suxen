package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

func TestDrainTargetRetirementIsSerializedOnBothDialects(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		serial := time.Now().UnixNano()
		source := fmt.Sprintf("drain-source-%d", serial)
		target := fmt.Sprintf("drain-target-%d", serial)
		for i, name := range []string{source, target} {
			if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
				Name: name, Driver: "fs", PhysicalIdentity: fmt.Sprintf("%064x", serial+int64(i)),
				ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_PASS6_STORE"},
			}); err != nil {
				t.Fatal(err)
			}
		}

		if err := metadata.BeginBlobStoreDrain(ctx, source, target); err != nil {
			t.Fatal(err)
		}
		if err := metadata.DeleteBlobStore(ctx, target, Ownership{}); !errors.Is(err, domain.ErrActiveDrainTarget) {
			t.Fatalf("active drain target deletion = %v", err)
		}
		current, err := metadata.BlobStore(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		changed := current
		changed.ConfigurationRef = &domain.ConfigurationReference{Env: "SUXEN_PASS6_REPLACEMENT"}
		changed.PhysicalIdentity = fmt.Sprintf("%064x", serial+3)
		if err := metadata.UpdateBlobStore(ctx, changed); !errors.Is(err, domain.ErrBlobStoreDefinitionImmutable) {
			t.Fatalf("active drain target reconfiguration = %v", err)
		}
		if err := metadata.SetBlobStoreState(ctx, source, domain.BlobStoreStateActive, ""); err != nil {
			t.Fatal(err)
		}
		if err := metadata.DeleteBlobStore(ctx, target, Ownership{}); err != nil {
			t.Fatalf("target still blocked after cancellation: %v", err)
		}

		// A simultaneous drain/deletion may choose either winner, but never
		// commit a source whose destination no longer exists.
		target = fmt.Sprintf("drain-race-%d", serial)
		if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
			Name: target, Driver: "fs", PhysicalIdentity: fmt.Sprintf("%064x", serial+2),
			ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_PASS6_RACE"},
		}); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		var drainErr, deleteErr error
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			drainErr = metadata.BeginBlobStoreDrain(ctx, source, target)
		}()
		go func() {
			defer wait.Done()
			<-start
			deleteErr = metadata.DeleteBlobStore(ctx, target, Ownership{})
		}()
		close(start)
		wait.Wait()
		if drainErr == nil {
			if !errors.Is(deleteErr, domain.ErrActiveDrainTarget) {
				t.Fatalf("accepted drain, deletion error = %v", deleteErr)
			}
			resolved, err := metadata.WriteBlobStore(ctx, source)
			if err != nil || resolved != target {
				t.Fatalf("accepted drain target = %q, %v", resolved, err)
			}
			if err := metadata.SetBlobStoreState(ctx, source, domain.BlobStoreStateDrained, target); err != nil {
				t.Fatal(err)
			}
			if err := metadata.DeleteBlobStore(ctx, target, Ownership{}); err != nil {
				t.Fatalf("completed migration without references still blocked: %v", err)
			}
		} else {
			if deleteErr != nil || !errors.Is(drainErr, domain.ErrInvalidDrainTarget) {
				t.Fatalf("deletion won: drain = %v, delete = %v", drainErr, deleteErr)
			}
		}

		// Opposite drains must lock the same rows in the same order. Exactly
		// one can begin, since its source then ceases to be an active target.
		left := fmt.Sprintf("drain-left-%d", serial)
		right := fmt.Sprintf("drain-right-%d", serial)
		for i, name := range []string{left, right} {
			if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
				Name: name, Driver: "fs", PhysicalIdentity: fmt.Sprintf("%064x", serial+int64(i)+4),
				ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_PASS6_OPPOSITE"},
			}); err != nil {
				t.Fatal(err)
			}
		}
		start = make(chan struct{})
		var leftErr, rightErr error
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			leftErr = metadata.BeginBlobStoreDrain(ctx, left, right)
		}()
		go func() {
			defer wait.Done()
			<-start
			rightErr = metadata.BeginBlobStoreDrain(ctx, right, left)
		}()
		close(start)
		wait.Wait()
		if !((leftErr == nil && errors.Is(rightErr, domain.ErrInvalidDrainTarget)) ||
			(rightErr == nil && errors.Is(leftErr, domain.ErrInvalidDrainTarget))) {
			t.Fatalf("opposite drains: left = %v, right = %v", leftErr, rightErr)
		}
	})
}

func TestDrainDestinationCannotStartAnotherDrain(t *testing.T) {
	forEachDialect(t, func(t *testing.T, metadata *SQLStore, backend string) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		serial := time.Now().UnixNano()
		create := func(label string, offset int64) string {
			name := fmt.Sprintf("drain-case-%s-%d", label, serial)
			if err := metadata.CreateBlobStore(ctx, domain.BlobStore{
				Name: name, Driver: "fs", PhysicalIdentity: fmt.Sprintf("%064x", serial+offset),
				ConfigurationRef: &domain.ConfigurationReference{Env: "SUXEN_PASS7_STORE"},
			}); err != nil {
				t.Fatal(err)
			}
			return name
		}
		source, middle, target := create("source", 1), create("middle", 2), create("target", 3)
		if err := metadata.BeginBlobStoreDrain(ctx, source, middle); err != nil {
			t.Fatal(err)
		}
		if err := metadata.BeginBlobStoreDrain(ctx, middle, target); !errors.Is(err, domain.ErrActiveDrainTarget) {
			t.Fatalf("drain destination became a source: %v", err)
		}
		for name, wantState := range map[string]string{
			source: domain.BlobStoreStateDraining,
			middle: domain.BlobStoreStateActive,
			target: domain.BlobStoreStateActive,
		} {
			current, err := metadata.BlobStore(ctx, name)
			if err != nil || current.State != wantState {
				t.Fatalf("%s state = %q, %v; want %q", name, current.State, err, wantState)
			}
		}
		if resolved, err := metadata.WriteBlobStore(ctx, source); err != nil || resolved != middle {
			t.Fatalf("accepted drain lost its target: %q, %v", resolved, err)
		}
		if err := metadata.SetBlobStoreState(ctx, source, domain.BlobStoreStateDrained, middle); err != nil {
			t.Fatal(err)
		}
		if err := metadata.BeginBlobStoreDrain(ctx, middle, target); err != nil {
			t.Fatalf("completed incoming drain still blocked destination: %v", err)
		}
		if err := metadata.SetBlobStoreState(ctx, middle, domain.BlobStoreStateActive, ""); err != nil {
			t.Fatal(err)
		}
		if err := metadata.SetBlobStoreState(ctx, source, domain.BlobStoreStateActive, ""); err != nil {
			t.Fatal(err)
		}
		if err := metadata.BeginBlobStoreDrain(ctx, source, middle); err != nil {
			t.Fatalf("cancellation left source blocked: %v", err)
		}

		// Both possible winners are valid; sharing the middle row must
		// prevent both transitions from committing as a chain.
		a, b, c := create("race-a", 4), create("race-b", 5), create("race-c", 6)
		start := make(chan struct{})
		var wait sync.WaitGroup
		var firstErr, secondErr error
		wait.Add(2)
		go func() { defer wait.Done(); <-start; firstErr = metadata.BeginBlobStoreDrain(ctx, a, b) }()
		go func() { defer wait.Done(); <-start; secondErr = metadata.BeginBlobStoreDrain(ctx, b, c) }()
		close(start)
		wait.Wait()
		if !((firstErr == nil && errors.Is(secondErr, domain.ErrActiveDrainTarget)) ||
			(secondErr == nil && errors.Is(firstErr, domain.ErrInvalidDrainTarget))) {
			t.Fatalf("concurrent chain: %v, %v", firstErr, secondErr)
		}
	})
}
