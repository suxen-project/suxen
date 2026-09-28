package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

type boundedHistoryStore struct {
	store.Store
	taskCalls     int
	deliveryCalls int
	maximumLimit  int
}

func (metadata *boundedHistoryStore) TaskPage(
	_ context.Context,
	snapshotID int64,
	beforeID int64,
	limit int,
) (store.IDPage[domain.Task], error) {
	metadata.taskCalls++
	metadata.maximumLimit = max(metadata.maximumLimit, limit)
	return syntheticIDPage(snapshotID, beforeID, limit, func(id int64) domain.Task {
		return domain.Task{ID: id}
	}), nil
}

func (metadata *boundedHistoryStore) WebhookDeliveryPage(
	_ context.Context,
	_ string,
	snapshotID int64,
	beforeID int64,
	limit int,
) (store.IDPage[domain.WebhookDelivery], error) {
	metadata.deliveryCalls++
	metadata.maximumLimit = max(metadata.maximumLimit, limit)
	return syntheticIDPage(snapshotID, beforeID, limit, func(id int64) domain.WebhookDelivery {
		return domain.WebhookDelivery{ID: id}
	}), nil
}

func syntheticIDPage[T any](
	snapshotID int64,
	beforeID int64,
	limit int,
	value func(int64) T,
) store.IDPage[T] {
	if snapshotID == 0 {
		snapshotID = 1001
	}
	startID := snapshotID
	if beforeID > 0 {
		startID = beforeID - 1
	}
	items := make([]T, 0, limit)
	for id := startID; id > 0 && len(items) < limit; id-- {
		items = append(items, value(id))
	}
	return store.IDPage[T]{
		Items:      items,
		SnapshotID: snapshotID,
		HasMore:    len(items) > 0 && startID-int64(len(items)) > 0,
	}
}

func TestTaskAndDeliveryHandlersPageBeyondOneThousandWithBoundedStoreCalls(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	metadata := &boundedHistoryStore{Store: fixture.Metadata}
	fixture.Handler.setMetadata(metadata)

	assertPagedEndpointCount[domain.Task](t, fixture, "/api/v1/tasks", 1001)
	assertPagedEndpointCount[domain.WebhookDelivery](
		t,
		fixture,
		"/api/v1/webhooks/scanner/deliveries",
		1001,
	)
	if metadata.taskCalls != 6 || metadata.deliveryCalls != 6 {
		t.Fatalf("page calls = tasks %d, deliveries %d; want 6 each", metadata.taskCalls, metadata.deliveryCalls)
	}
	if metadata.maximumLimit > httpx.MaximumCollectionLimit {
		t.Fatalf("store page limit = %d, want at most %d", metadata.maximumLimit, httpx.MaximumCollectionLimit)
	}
}

func assertPagedEndpointCount[T any](
	t *testing.T,
	fixture *serverFixture,
	requestPath string,
	want int,
) {
	t.Helper()
	cursor := ""
	count := 0
	for {
		pagePath := requestPath + "?limit=200"
		if cursor != "" {
			pagePath += "&cursor=" + url.QueryEscape(cursor)
		}
		response := fixture.request(t, http.MethodGet, pagePath, nil, true)
		assertStatus(t, response, http.StatusOK)
		var page httpx.CollectionPage[T]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if len(page.Items) > httpx.MaximumCollectionLimit {
			t.Fatalf("response page contains %d items", len(page.Items))
		}
		count += len(page.Items)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if count != want {
		t.Fatalf("endpoint returned %d items, want %d", count, want)
	}
}
