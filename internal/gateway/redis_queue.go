package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	outboundStream = "telegram_gateway:outbound"
	outboundGroup  = "telegram_gateway:outbound_workers"
)

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type RedisQueue struct {
	client      *redis.Client
	stream      string
	group       string
	index       string
	dlq         string
	dlqIndex    string
	replayIndex string
	claimMu     sync.Mutex
	claimCursor string
}

func NewRedisQueue(ctx context.Context, config RedisConfig) (*RedisQueue, error) {
	if strings.TrimSpace(config.Addr) == "" {
		return nil, errors.New("Redis address is required")
	}
	client := redis.NewClient(&redis.Options{Addr: config.Addr, Password: config.Password, DB: config.DB})
	queue := newRedisQueue(client, "telegram_gateway")
	if err := queue.verify(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.XGroupCreateMkStream(ctx, queue.stream, queue.group, "0").Err(); err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		_ = client.Close()
		return nil, fmt.Errorf("initialize outbound stream: %w", err)
	}
	return queue, nil
}

func newRedisQueue(client *redis.Client, namespace string) *RedisQueue {
	return &RedisQueue{client: client, stream: namespace + ":outbound", group: namespace + ":outbound_workers",
		index: namespace + ":outbound:accepted", dlq: namespace + ":outbound:dlq", dlqIndex: namespace + ":outbound:dlq:index",
		replayIndex: namespace + ":outbound:replay:index"}
}

func (q *RedisQueue) verify(ctx context.Context) error {
	if err := q.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("connect to Redis: %w", err)
	}
	serverInfo, err := q.client.Info(ctx, "server").Result()
	if err != nil {
		return fmt.Errorf("read Redis server information: %w", err)
	}
	version := infoValue(serverInfo, "redis_version")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return fmt.Errorf("Redis returned an invalid version")
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major < 6 || (major == 6 && minor < 2) {
		return fmt.Errorf("Redis 6.2 or newer is required")
	}
	persistenceInfo, err := q.client.Info(ctx, "persistence").Result()
	if err != nil {
		return fmt.Errorf("read Redis persistence information: %w", err)
	}
	if infoValue(persistenceInfo, "aof_enabled") != "1" {
		return fmt.Errorf("Redis AOF persistence must be enabled")
	}
	return nil
}

func infoValue(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return ""
}

var appendOnceScript = redis.NewScript(`
local existing = redis.call('HGET', KEYS[2], ARGV[1])
if existing then
  return existing
end
local stream_id = redis.call('XADD', KEYS[1], '*', 'delivery_id', ARGV[1], 'direction', 'outbound')
redis.call('HSET', KEYS[2], ARGV[1], stream_id)
return stream_id
`)

func (q *RedisQueue) Append(ctx context.Context, deliveryID string) (string, error) {
	result, err := appendOnceScript.Run(ctx, q.client, []string{q.stream, q.index}, deliveryID).Result()
	if err != nil {
		return "", err
	}
	streamID, ok := result.(string)
	if !ok || streamID == "" {
		return "", errors.New("Redis returned an invalid stream ID")
	}
	return streamID, nil
}

func (q *RedisQueue) Read(ctx context.Context, consumer string, count int64) ([]QueueMessage, error) {
	streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: q.group, Consumer: consumer, Streams: []string{q.stream, ">"}, Count: count, Block: time.Second,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var messages []QueueMessage
	for _, stream := range streams {
		for _, message := range stream.Messages {
			deliveryID, _ := message.Values["delivery_id"].(string)
			if deliveryID != "" {
				messages = append(messages, QueueMessage{ID: message.ID, DeliveryID: deliveryID})
			}
		}
	}
	return messages, nil
}

func (q *RedisQueue) ClaimStale(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]QueueMessage, error) {
	q.claimMu.Lock()
	defer q.claimMu.Unlock()
	start := q.claimCursor
	if start == "" {
		start = "0-0"
	}
	messages, next, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: q.stream, Group: q.group, Consumer: consumer, MinIdle: minIdle, Start: start, Count: count,
	}).Result()
	if errors.Is(err, redis.Nil) || (err != nil && ctx.Err() != nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	q.claimCursor = next
	if q.claimCursor == "" {
		q.claimCursor = "0-0"
	}
	result := make([]QueueMessage, 0, len(messages))
	for _, message := range messages {
		deliveryID, _ := message.Values["delivery_id"].(string)
		if deliveryID != "" {
			result = append(result, QueueMessage{ID: message.ID, DeliveryID: deliveryID})
		}
	}
	return result, nil
}

func (q *RedisQueue) Ack(ctx context.Context, streamID string) error {
	return q.client.XAck(ctx, q.stream, q.group, streamID).Err()
}

var moveOutboundToDLQ = redis.NewScript(`
local existing = redis.call('HGET', KEYS[3], ARGV[2])
if existing then
  redis.call('XACK', KEYS[1], ARGV[6], ARGV[1])
  return existing
end
local id = redis.call('XADD', KEYS[2], '*',
  'delivery_id', ARGV[2],
  'source_stream_id', ARGV[1],
  'route_key', ARGV[3],
  'attempt_count', ARGV[4],
  'error_class', ARGV[5])
redis.call('HSET', KEYS[3], ARGV[2], id)
redis.call('XACK', KEYS[1], ARGV[6], ARGV[1])
return id
`)

func (q *RedisQueue) MoveToDLQAndAck(ctx context.Context, message QueueMessage, metadata DLQMetadata) error {
	return moveOutboundToDLQ.Run(ctx, q.client, []string{q.stream, q.dlq, q.dlqIndex}, message.ID, message.DeliveryID,
		metadata.RouteKey, metadata.AttemptCount, metadata.ErrorClass, q.group).Err()
}

var replayOutboundDLQ = redis.NewScript(`
local replay_key = ARGV[1] .. ':' .. ARGV[2]
local existing = redis.call('HGET', KEYS[3], replay_key)
if existing then
  return existing
end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[4], ARGV[1])
local id = redis.call('XADD', KEYS[1], '*', 'delivery_id', ARGV[1], 'direction', 'outbound')
redis.call('HSET', KEYS[2], ARGV[1], id)
redis.call('HSET', KEYS[3], replay_key, id)
return id
`)

func (q *RedisQueue) ReplayFromDLQ(ctx context.Context, deliveryID string, generation int) (string, error) {
	return replayOutboundDLQ.Run(ctx, q.client, []string{q.stream, q.index, q.replayIndex, q.dlqIndex},
		deliveryID, generation).Text()
}

func (q *RedisQueue) InspectQueue(ctx context.Context) (QueueObservation, error) {
	result := QueueObservation{CheckedAt: time.Now().UTC()}
	if err := q.client.Ping(ctx).Err(); err != nil {
		return result, err
	}
	result.Available = true
	persistence, err := q.client.Info(ctx, "persistence").Result()
	if err != nil {
		return result, err
	}
	result.Persistence = infoValue(persistence, "aof_enabled") == "1"
	pending, err := q.client.XPending(ctx, q.stream, q.group).Result()
	if err != nil && !strings.Contains(err.Error(), "NOGROUP") {
		return result, err
	}
	if pending != nil {
		result.Pending = pending.Count
	}
	groups, err := q.client.XInfoGroups(ctx, q.stream).Result()
	if err != nil && !strings.Contains(err.Error(), "no such key") {
		return result, err
	}
	for _, group := range groups {
		if group.Name == q.group {
			result.Depth = group.Lag + group.Pending
			break
		}
	}
	return result, nil
}

func (q *RedisQueue) Close() error { return q.client.Close() }
