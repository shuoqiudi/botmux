package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

func inspectRedisQueue(ctx context.Context, client redis.UniversalClient, stream, group string) (QueueObservation, error) {
	result := QueueObservation{CheckedAt: time.Now().UTC()}
	if err := client.Ping(ctx).Err(); err != nil {
		return result, err
	}
	result.Available = true
	persistence, err := client.Info(ctx, "persistence").Result()
	if err != nil {
		return result, err
	}
	result.Persistence = infoValue(persistence, "aof_enabled") == "1"
	pending, err := client.XPending(ctx, stream, group).Result()
	if err != nil && !strings.Contains(err.Error(), "NOGROUP") {
		return result, err
	}
	if pending != nil {
		result.Pending = pending.Count
	}
	groups, err := client.XInfoGroups(ctx, stream).Result()
	if err != nil && !strings.Contains(err.Error(), "no such key") {
		return result, err
	}
	for _, candidate := range groups {
		if candidate.Name == group {
			result.Depth = candidate.Lag + candidate.Pending
			break
		}
	}
	return result, nil
}

func verifyRedisAOF(ctx context.Context, client redis.UniversalClient) error {
	info, err := client.Info(ctx, "persistence").Result()
	if err != nil {
		return fmt.Errorf("read Redis persistence status: %w", err)
	}
	if infoValue(info, "aof_enabled") != "1" {
		return errors.New("Redis AOF persistence is required for Gateway delivery")
	}
	return nil
}
