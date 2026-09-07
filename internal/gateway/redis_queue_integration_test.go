package gateway

import (
	"context"
	"flag"
	"testing"
	"time"
)

var redisIntegrationAddr = flag.String("gateway-redis-test-addr", "", "address of a disposable Redis >=6.2 with AOF enabled")

func TestRedisQueueDurableAppendAndAck(t *testing.T) {
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
		messages, readErr := queue.Read(ctx, "integration-worker", 32)
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
	if err := queue.Ack(ctx, found.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := queue.client.XPending(ctx, outboundStream, outboundGroup).Result()
	if err != nil {
		t.Fatal(err)
	}
	if pending.Count != 0 {
		t.Fatalf("stream item remained pending after XACK: %d", pending.Count)
	}
}
