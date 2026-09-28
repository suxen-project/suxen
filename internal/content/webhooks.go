package content

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/contract"
	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

const (
	webhookBatchSize       = 20
	webhookDeliveryLease   = 30 * time.Second
	webhookDeliveryTimeout = 15 * time.Second
	// Defaults used when a Runtime is built with an unset Config (e.g. in tests);
	// config.Load supplies operator-tunable values in a running server.
	defaultWebhookRetryBase    = time.Second
	defaultWebhookPollInterval = time.Second
	defaultWebhookMaxAttempts  = 8
)

func (rt *Runtime) webhookPollInterval() time.Duration {
	if rt.Config.WebhookPollInterval > 0 {
		return rt.Config.WebhookPollInterval
	}
	return defaultWebhookPollInterval
}

func (rt *Runtime) webhookMaximumAttempts() int {
	if rt.Config.WebhookMaxAttempts > 0 {
		return rt.Config.WebhookMaxAttempts
	}
	return defaultWebhookMaxAttempts
}

func (rt *Runtime) webhookRetryBase() time.Duration {
	if rt.Config.WebhookRetryBase > 0 {
		return rt.Config.WebhookRetryBase
	}
	return defaultWebhookRetryBase
}

func (rt *Runtime) WebhookDeliveryWorker(ctx context.Context) {
	rt.drainWebhookDeliveries(ctx)
	ticker := time.NewTicker(rt.webhookPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.drainWebhookDeliveries(ctx)
		}
	}
}

func (rt *Runtime) drainWebhookDeliveries(ctx context.Context) {
	for ctx.Err() == nil {
		count, err := rt.DeliverWebhookBatch(ctx)
		if err != nil {
			rt.Log.Error("deliver webhook batch", "error", err)
			return
		}
		if count < webhookBatchSize {
			return
		}
	}
}

func (rt *Runtime) DeliverWebhookBatch(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	deliveries, err := rt.webhookOutbox().ClaimWebhookDeliveries(
		ctx,
		rt.leaderID(),
		now,
		webhookDeliveryLease,
		webhookBatchSize,
	)
	if err != nil {
		return 0, err
	}
	var waitGroup sync.WaitGroup
	for _, delivery := range deliveries {
		waitGroup.Add(1)
		go func(webhookDelivery domain.WebhookDelivery) {
			defer waitGroup.Done()
			rt.deliverWebhook(ctx, webhookDelivery)
		}(delivery)
	}
	waitGroup.Wait()
	return len(deliveries), nil
}

func (rt *Runtime) deliverWebhook(
	ctx context.Context,
	delivery domain.WebhookDelivery,
) {
	deliveryContext, cancel := context.WithTimeout(ctx, webhookDeliveryTimeout)
	defer cancel()

	err := rt.SendWebhookRequest(deliveryContext, delivery)
	status := "delivered"
	nextAttemptAt := time.Now().UTC()
	lastError := ""
	if err != nil {
		lastError = err.Error()
		if delivery.Attempts >= rt.webhookMaximumAttempts() {
			status = "dead"
		} else {
			status = "retry"
			nextAttemptAt = nextAttemptAt.Add(rt.webhookRetryDelay(delivery.Attempts))
		}
	}
	rt.observeWebhookDelivery(status)
	if completionErr := rt.webhookOutbox().CompleteWebhookDelivery(
		ctx,
		delivery.ID,
		rt.leaderID(),
		status,
		nextAttemptAt,
		lastError,
	); completionErr != nil {
		rt.Log.Error(
			"complete webhook delivery",
			"delivery", delivery.ID,
			"error", completionErr,
		)
	}
}

func (rt *Runtime) SendWebhookRequest(
	ctx context.Context,
	delivery domain.WebhookDelivery,
) error {
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		delivery.TargetURL,
		bytes.NewReader(delivery.Payload),
	)
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", rt.WebhookUserAgent)
	request.Header.Set("X-Suxen-Delivery", strconv.FormatInt(delivery.ID, 10))
	request.Header.Set("X-Suxen-Event", delivery.Event)
	request.Header.Set("X-Suxen-Webhook-Version", contract.WebhookAPIVersion())
	request.Header.Set("X-Suxen-Signature-256", webhookSignature(
		delivery.Secret,
		delivery.Payload,
	))

	response, err := rt.webhookClient().Do(request)
	if err != nil {
		return fmt.Errorf("send webhook request: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if readErr != nil {
		return fmt.Errorf("read webhook response: %w", readErr)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"webhook returned %s: %s",
			response.Status,
			bytes.TrimSpace(responseBody),
		)
	}
	return nil
}

func webhookSignature(secret string, payload []byte) string {
	signature := hmac.New(sha256.New, []byte(secret))
	_, _ = signature.Write(payload)
	return "sha256=" + hex.EncodeToString(signature.Sum(nil))
}

func (rt *Runtime) webhookRetryDelay(attempt int) time.Duration {
	delay := rt.webhookRetryBase()
	if delay >= time.Hour {
		return time.Hour
	}
	// Saturate before multiplying. Even a one-nanosecond base reaches the
	// cap in 42 doublings, so very large attempt counts remain bounded.
	for attempt > 1 {
		if delay >= time.Hour/2 {
			return time.Hour
		}
		delay *= 2
		attempt--
	}
	return delay
}

func (rt *Runtime) EnqueueAssetEvent(
	ctx context.Context,
	eventType string,
	asset domain.Asset,
) {
	repository, err := rt.repositoryResolver().Repository(ctx, asset.Repository)
	if err != nil {
		rt.Log.Error(
			"project webhook asset",
			"event", eventType,
			"repository", asset.Repository,
			"asset", asset.ID,
			"error", err,
		)
		return
	}
	asset.Attributes = assetattrs.Project(asset, repository)
	rt.EnqueueWebhookEvent(ctx, domain.WebhookEvent{
		ID:         httpx.RandomSecret(18),
		Type:       eventType,
		Repository: asset.Repository,
		OccurredAt: time.Now().UTC(),
		Asset:      &asset,
	})
}

func (rt *Runtime) EnqueueWebhookEvent(
	ctx context.Context,
	event domain.WebhookEvent,
) {
	if err := rt.webhookOutbox().EnqueueWebhookEvent(ctx, event); err != nil {
		rt.Log.Error(
			"enqueue webhook event",
			"event", event.Type,
			"repository", event.Repository,
			"error", err,
		)
	}
}
