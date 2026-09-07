package gateway

import (
	"context"
	"flag"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

var redisIntegrationAddr = flag.String("gateway-redis-test-addr", "", "address of a disposable Redis >=6.2 with AOF enabled")

func TestRedisQueueDurableReclaimAndDLQMove(t *testing.T) {
	if *redisIntegrationAddr == "" {
		t.Skip("pass -gateway-redis-test-addr to run the real Redis integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queue, err := NewRedisQueue(ctx, RedisConfig{Addr: *redisIntegrationAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()

	deliveryID := "integration-" + newOpaqueID()
	first, err := queue.Append(ctx, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := queue.Append(ctx, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent append returned %q then %q", first, second)
	}

	var found QueueMessage
	deadline := time.Now().Add(4 * time.Second)
	for found.DeliveryID == "" && time.Now().Before(deadline) {
		messages, readErr := queue.Read(ctx, "abandoned-worker", 32)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, message := range messages {
			if message.DeliveryID == deliveryID {
				found = message
				break
			}
		}
	}
	if found.DeliveryID != deliveryID || found.ID != first {
		t.Fatalf("durable stream message not received: %+v", found)
	}
	claimed, err := queue.ClaimStale(ctx, "restart-worker", 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	var reclaimed QueueMessage
	for _, message := range claimed {
		if message.DeliveryID == deliveryID {
			reclaimed = message
		}
	}
	if reclaimed != found {
		t.Fatalf("XAUTOCLAIM did not recover abandoned message: got %+v want %+v", reclaimed, found)
	}
	metadata := DLQMetadata{RouteKey: "safe-route", AttemptCount: 5, ErrorClass: "telegram_unavailable"}
	if err := queue.MoveToDLQAndAck(ctx, reclaimed, metadata); err != nil {
		t.Fatal(err)
	}
	dlqEntries, err := queue.client.XRevRangeN(ctx, queue.dlq, "+", "-", 20).Result()
	if err != nil {
		t.Fatal(err)
	}
	foundDLQ := false
	for _, entry := range dlqEntries {
		if entry.Values["delivery_id"] == deliveryID && entry.Values["source_stream_id"] == first &&
			entry.Values["route_key"] == metadata.RouteKey && entry.Values["error_class"] == metadata.ErrorClass &&
			entry.Values["attempt_count"] == "5" {
			foundDLQ = true
		}
	}
	if !foundDLQ {
		t.Fatalf("durable DLQ metadata not found: %+v", dlqEntries)
	}
	pending, err := queue.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: queue.stream, Group: queue.group, Start: first, End: first, Count: 1,
	}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("source remained pending after durable DLQ append: %+v", pending)
	}
}
