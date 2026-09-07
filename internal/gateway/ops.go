package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

type OperationsStore interface {
	GetGatewayRouteMetrics(context.Context, string, time.Time) (*models.GatewayRouteMetrics, error)
	GetGatewayDeliveryDetail(context.Context, string, string) (*models.GatewayDeliveryDetail, error)
	ListGatewayDLQ(context.Context, string, string, int) ([]models.GatewayDLQItem, error)
	PrepareGatewayDLQReplay(context.Context, string, string) (*models.GatewayDLQItem, bool, error)
	CompleteGatewayDLQReplay(context.Context, string, string, string, string) error
	DiscardGatewayDLQ(context.Context, string, string) (*models.GatewayDLQItem, bool, error)
	GetGatewayRouteProbeTarget(context.Context, string) (*models.GatewayRouteProbeTarget, error)
}

type WorkerProbe interface {
	WorkerHealth() (bool, time.Time)
}

type Operations struct {
	store         OperationsStore
	outboundQueue OperationalQueue
	inboundQueue  OperationalQueue
	outbound      WorkerProbe
	inbound       WorkerProbe
	telegram      TelegramProbe
	backendClient *http.Client
}

type TelegramProbe interface {
	ProbeBot(context.Context, string) error
	ProbeDestination(context.Context, string, int64) error
}

func NewOperations(repository OperationsStore, outboundQueue, inboundQueue OperationalQueue, outbound, inbound WorkerProbe, telegram TelegramProbe, backendClient *http.Client) *Operations {
	if backendClient == nil {
		backendClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Operations{store: repository, outboundQueue: outboundQueue, inboundQueue: inboundQueue, outbound: outbound, inbound: inbound,
		telegram: telegram, backendClient: backendClient}
}

func (o *Operations) Health(ctx context.Context) models.GatewayHealth {
	now := time.Now().UTC()
	result := models.GatewayHealth{
		WebApp:         models.GatewayComponentHealth{Status: "healthy", CheckedAt: now.Format(time.RFC3339Nano)},
		InboundWorker:  workerSnapshot(o.inbound, now),
		OutboundWorker: workerSnapshot(o.outbound, now),
		CheckedAt:      now.Format(time.RFC3339Nano),
	}
	observations := make([]QueueObservation, 0, 2)
	for _, queue := range []OperationalQueue{o.outboundQueue, o.inboundQueue} {
		if queue == nil {
			continue
		}
		observation, err := queue.InspectQueue(ctx)
		if err != nil {
			result.Redis = models.GatewayQueueHealth{GatewayComponentHealth: models.GatewayComponentHealth{
				Status: "unhealthy", Detail: "redis_unavailable", CheckedAt: now.Format(time.RFC3339Nano),
			}}
			return result
		}
		observations = append(observations, observation)
	}
	if len(observations) == 0 {
		result.Redis = models.GatewayQueueHealth{GatewayComponentHealth: models.GatewayComponentHealth{Status: "unavailable", Detail: "not_configured"}}
		return result
	}
	result.Redis = models.GatewayQueueHealth{GatewayComponentHealth: models.GatewayComponentHealth{Status: "healthy", CheckedAt: now.Format(time.RFC3339Nano)}, Persistence: "aof"}
	for _, observation := range observations {
		result.Redis.Depth += observation.Depth
		result.Redis.Pending += observation.Pending
		if !observation.Persistence {
			result.Redis.Status = "unhealthy"
			result.Redis.Detail = "aof_disabled"
			result.Redis.Persistence = "disabled"
		}
	}
	return result
}

func workerSnapshot(probe WorkerProbe, now time.Time) models.GatewayWorkerHealth {
	if probe == nil {
		return models.GatewayWorkerHealth{Status: "unavailable"}
	}
	running, heartbeat := probe.WorkerHealth()
	result := models.GatewayWorkerHealth{Status: "stopped"}
	if !heartbeat.IsZero() {
		result.LastHeartbeat = heartbeat.UTC().Format(time.RFC3339Nano)
	}
	if running {
		result.Status = "healthy"
		if now.Sub(heartbeat) > 10*time.Second {
			result.Status = "degraded"
		}
	}
	return result
}

func (o *Operations) RouteMetrics(ctx context.Context, routeKey string) (*models.GatewayRouteMetrics, error) {
	now := time.Now().UTC()
	result, err := o.store.GetGatewayRouteMetrics(ctx, routeKey, now)
	if err != nil {
		return nil, err
	}
	health := o.Health(ctx)
	result.Components.Redis = health.Redis.GatewayComponentHealth
	result.Components.InboundWorker = models.GatewayComponentHealth{Status: health.InboundWorker.Status, CheckedAt: health.InboundWorker.LastHeartbeat}
	result.Components.OutboundWorker = models.GatewayComponentHealth{Status: health.OutboundWorker.Status, CheckedAt: health.OutboundWorker.LastHeartbeat}
	target, err := o.store.GetGatewayRouteProbeTarget(ctx, routeKey)
	if err != nil {
		return nil, err
	}
	checkedAt := now.Format(time.RFC3339Nano)
	if o.telegram == nil {
		result.Components.BotAuthentication = models.GatewayComponentHealth{Status: "unavailable", Detail: "probe_not_configured"}
		result.Components.DestinationValidation = models.GatewayComponentHealth{Status: "unavailable", Detail: "probe_not_configured"}
	} else if err := o.telegram.ProbeBot(ctx, target.Token); err != nil {
		result.Components.BotAuthentication = models.GatewayComponentHealth{Status: "unhealthy", Detail: "telegram_auth_failed", CheckedAt: checkedAt}
		result.Components.DestinationValidation = models.GatewayComponentHealth{Status: "unknown", Detail: "bot_auth_failed", CheckedAt: checkedAt}
	} else {
		result.Components.BotAuthentication = models.GatewayComponentHealth{Status: "healthy", CheckedAt: checkedAt}
		if err := o.telegram.ProbeDestination(ctx, target.Token, target.ChatID); err != nil {
			result.Components.DestinationValidation = models.GatewayComponentHealth{Status: "unhealthy", Detail: "destination_unreachable", CheckedAt: checkedAt}
		} else {
			result.Components.DestinationValidation = models.GatewayComponentHealth{Status: "healthy", CheckedAt: checkedAt}
		}
	}
	if target.BackendHealthURL == "" {
		result.Components.BackendHealth = models.GatewayComponentHealth{Status: "unavailable", Detail: "health_endpoint_not_configured"}
	} else {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, target.BackendHealthURL, nil)
		if requestErr != nil {
			result.Components.BackendHealth = models.GatewayComponentHealth{Status: "unhealthy", Detail: "health_endpoint_invalid", CheckedAt: checkedAt}
		} else {
			if target.BackendToken != "" {
				request.Header.Set("Authorization", "Bearer "+target.BackendToken)
			}
			response, requestErr := o.backendClient.Do(request)
			if requestErr != nil {
				result.Components.BackendHealth = models.GatewayComponentHealth{Status: "unhealthy", Detail: "backend_unavailable", CheckedAt: checkedAt}
			} else {
				_ = response.Body.Close()
				status := "unhealthy"
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					status = "healthy"
				}
				result.Components.BackendHealth = models.GatewayComponentHealth{Status: status, Detail: "health_check", CheckedAt: checkedAt}
			}
		}
	}
	return result, nil
}

func (o *Operations) DeliveryDetail(ctx context.Context, routeKey, deliveryID string) (*models.GatewayDeliveryDetail, error) {
	return o.store.GetGatewayDeliveryDetail(ctx, routeKey, deliveryID)
}

func (o *Operations) ListDLQ(ctx context.Context, routeKey, state string, limit int) ([]models.GatewayDLQItem, error) {
	return o.store.ListGatewayDLQ(ctx, routeKey, state, limit)
}

func (o *Operations) ReplayDLQ(ctx context.Context, deliveryID, actorID string) (*models.GatewayDLQItem, bool, error) {
	item, unchanged, err := o.store.PrepareGatewayDLQReplay(ctx, deliveryID, actorID)
	if err != nil || unchanged {
		return item, unchanged, err
	}
	var queue OperationalQueue
	if item.Direction == models.GatewayDirectionOutbound {
		queue = o.outboundQueue
	} else {
		queue = o.inboundQueue
	}
	if queue == nil {
		return nil, false, ErrQueueUnavailable
	}
	streamID, err := queue.ReplayFromDLQ(ctx, deliveryID, item.ReplayCount)
	if err != nil {
		return nil, false, ErrQueueUnavailable
	}
	if err := o.store.CompleteGatewayDLQReplay(ctx, deliveryID, string(item.Direction), streamID, actorID); err != nil {
		return nil, false, err
	}
	item.Status = models.GatewayDLQReplayed
	item.ActedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return item, false, nil
}

func (o *Operations) DiscardDLQ(ctx context.Context, deliveryID, actorID string) (*models.GatewayDLQItem, bool, error) {
	return o.store.DiscardGatewayDLQ(ctx, deliveryID, actorID)
}

func IsDLQNotFound(err error) bool { return errors.Is(err, store.ErrGatewayDLQNotFound) }
func IsDLQConflict(err error) bool {
	return errors.Is(err, store.ErrGatewayDLQDiscarded) || errors.Is(err, store.ErrGatewayDLQReplayed)
}
