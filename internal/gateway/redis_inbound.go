package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisInboundQueue struct {
	client      redis.UniversalClient
	stream      string
	group       string
	index       string
	dlq         string
	dlqIndex    string
	replayIndex string
}

func NewRedisInboundQueue(client redis.UniversalClient) *RedisInboundQueue {
	return NewRedisInboundQueueWithNamespace(client, "gateway")
}

// NewRedisInboundQueueWithNamespace isolates integration suites while the
// production constructor retains stable Stream names.
func NewRedisInboundQueueWithNamespace(client redis.UniversalClient, namespace string) *RedisInboundQueue {
	return &RedisInboundQueue{client: client, stream: namespace + ":inbound", group: namespace + ":inbound:workers",
		index: namespace + ":inbound:index", dlq: namespace + ":inbound:dlq", dlqIndex: namespace + ":inbound:dlq:index",
		replayIndex: namespace + ":inbound:replay:index"}
}

var appendInboundOnce = redis.NewScript(`
local existing = redis.call('HGET', KEYS[2], ARGV[1])
if existing then
  return existing
end
local id = redis.call('XADD', KEYS[1], '*', 'delivery_id', ARGV[1])
redis.call('HSET', KEYS[2], ARGV[1], id)
return id
`)

func (q *RedisInboundQueue) AppendUnique(ctx context.Context, deliveryID string) (string, error) {
	result, err := appendInboundOnce.Run(ctx, q.client, []string{q.stream, q.index}, deliveryID).Text()
	if err != nil {
		return "", err
	}
	return result, nil
}

func (q *RedisInboundQueue) EnsureGroup(ctx context.Context) error {
	err := q.client.XGroupCreateMkStream(ctx, q.stream, q.group, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

func (q *RedisInboundQueue) ReadNew(ctx context.Context, consumer string, block time.Duration, count int64) ([]StreamMessage, error) {
	streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: q.group, Consumer: consumer, Streams: []string{q.stream, ">"}, Count: count, Block: block,
	}).Result()
	if errors.Is(err, redis.Nil) || (err != nil && ctx.Err() != nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return streamMessages(streams), nil
}

func (q *RedisInboundQueue) ClaimStale(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]StreamMessage, error) {
	messages, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: q.stream, Group: q.group, Consumer: consumer,
		MinIdle: minIdle, Start: "0-0", Count: count,
	}).Result()
	if errors.Is(err, redis.Nil) || (err != nil && ctx.Err() != nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return redisMessages(messages), nil
}

func streamMessages(streams []redis.XStream) []StreamMessage {
	var result []StreamMessage
	for _, stream := range streams {
		result = append(result, redisMessages(stream.Messages)...)
	}
	return result
}

func redisMessages(messages []redis.XMessage) []StreamMessage {
	result := make([]StreamMessage, 0, len(messages))
	for _, message := range messages {
		deliveryID, _ := message.Values["delivery_id"].(string)
		if deliveryID != "" {
			result = append(result, StreamMessage{ID: message.ID, DeliveryID: deliveryID})
		}
	}
	return result
}

func (q *RedisInboundQueue) Ack(ctx context.Context, streamID string) error {
	return q.client.XAck(ctx, q.stream, q.group, streamID).Err()
}

var moveInboundToDLQ = redis.NewScript(`
local existing = redis.call('HGET', KEYS[3], ARGV[2])
if existing then
  redis.call('XACK', KEYS[1], ARGV[5], ARGV[1])
  return existing
end
local id = redis.call('XADD', KEYS[2], '*',
  'delivery_id', ARGV[2],
  'source_stream_id', ARGV[1],
  'error_class', ARGV[3],
  'attempt_count', ARGV[4])
redis.call('HSET', KEYS[3], ARGV[2], id)
redis.call('XACK', KEYS[1], ARGV[5], ARGV[1])
return id
`)

func (q *RedisInboundQueue) MoveToDLQAndAck(ctx context.Context, message StreamMessage, class string, attempts int) error {
	return moveInboundToDLQ.Run(ctx, q.client, []string{q.stream, q.dlq, q.dlqIndex},
		message.ID, message.DeliveryID, class, attempts, q.group).Err()
}

var replayInboundDLQ = redis.NewScript(`
local replay_key = ARGV[1] .. ':' .. ARGV[2]
local existing = redis.call('HGET', KEYS[3], replay_key)
if existing then
  return existing
end
redis.call('HDEL', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[4], ARGV[1])
local id = redis.call('XADD', KEYS[1], '*', 'delivery_id', ARGV[1], 'direction', 'inbound')
redis.call('HSET', KEYS[2], ARGV[1], id)
redis.call('HSET', KEYS[3], replay_key, id)
return id
`)

func (q *RedisInboundQueue) ReplayFromDLQ(ctx context.Context, deliveryID string, generation int) (string, error) {
	return replayInboundDLQ.Run(ctx, q.client, []string{q.stream, q.index, q.replayIndex, q.dlqIndex},
		deliveryID, generation).Text()
}

func (q *RedisInboundQueue) InspectQueue(ctx context.Context) (QueueObservation, error) {
	return inspectRedisQueue(ctx, q.client, q.stream, q.group)
}

// VerifyDurability rejects ephemeral Redis deployments before workers start.
// The production contract requires AOF and a persistent volume.
func (q *RedisInboundQueue) VerifyDurability(ctx context.Context) error {
	return verifyRedisAOF(ctx, q.client)
}
