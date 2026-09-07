package models

// GatewayComponentHealth keeps transport dependencies separate so a healthy
// web process cannot mask a broken delivery path.
type GatewayComponentHealth struct {
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

type GatewayWorkerHealth struct {
	Status        string `json:"status"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
}

type GatewayQueueHealth struct {
	GatewayComponentHealth
	Persistence string `json:"persistence,omitempty"`
	Depth       int64  `json:"depth"`
	Pending     int64  `json:"pending"`
}

type GatewayHealth struct {
	WebApp         GatewayComponentHealth `json:"web_app"`
	Redis          GatewayQueueHealth     `json:"redis"`
	InboundWorker  GatewayWorkerHealth    `json:"inbound_worker"`
	OutboundWorker GatewayWorkerHealth    `json:"outbound_worker"`
	CheckedAt      string                 `json:"checked_at"`
}

type GatewayRouteComponents struct {
	TelegramPolling       GatewayComponentHealth `json:"telegram_polling"`
	BotAuthentication     GatewayComponentHealth `json:"bot_authentication"`
	DestinationValidation GatewayComponentHealth `json:"destination_validation"`
	BackendHealth         GatewayComponentHealth `json:"backend_health"`
	Redis                 GatewayComponentHealth `json:"redis"`
	InboundWorker         GatewayComponentHealth `json:"inbound_worker"`
	OutboundWorker        GatewayComponentHealth `json:"outbound_worker"`
}

type GatewayLifecycleCounts struct {
	Accepted     int `json:"accepted"`
	Succeeded    int `json:"succeeded"`
	Retrying     int `json:"retrying"`
	Reconciling  int `json:"reconciling"`
	DeadLettered int `json:"dead_lettered"`
}

type GatewayRouteMetrics struct {
	RouteKey           string                 `json:"route_key"`
	Revision           int64                  `json:"revision"`
	Counts             GatewayLifecycleCounts `json:"counts"`
	QueueDepth         int                    `json:"queue_depth"`
	OldestPendingAgeMS int64                  `json:"oldest_pending_age_ms"`
	AttemptCount       int                    `json:"attempt_count"`
	DLQCount           int                    `json:"dlq_count"`
	AverageEndToEndMS  int64                  `json:"average_end_to_end_latency_ms"`
	MaximumEndToEndMS  int64                  `json:"maximum_end_to_end_latency_ms"`
	Components         GatewayRouteComponents `json:"components"`
	NativeBotID        int64                  `json:"-"`
}

// GatewayDeliveryAttempt is deliberately correlation-only. It contains no
// worker identity, HTTP body, destination, callback ID, or payload.
type GatewayDeliveryAttempt struct {
	AttemptID  string `json:"attempt_id"`
	Sequence   int    `json:"sequence"`
	Status     string `json:"status"`
	ErrorClass string `json:"error_class,omitempty"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at,omitempty"`
}

type GatewayDeliveryDetail struct {
	DeliveryID    string                   `json:"delivery_id"`
	RouteKey      string                   `json:"route_key"`
	RouteRevision int64                    `json:"route_revision"`
	Direction     string                   `json:"direction"`
	Action        string                   `json:"action"`
	Status        string                   `json:"status"`
	AttemptCount  int                      `json:"attempt_count"`
	ErrorClass    string                   `json:"error_class,omitempty"`
	CreatedAt     string                   `json:"created_at"`
	AcceptedAt    string                   `json:"accepted_at,omitempty"`
	NextAttemptAt string                   `json:"next_attempt_at,omitempty"`
	CompletedAt   string                   `json:"completed_at,omitempty"`
	UpdatedAt     string                   `json:"updated_at"`
	Attempts      []GatewayDeliveryAttempt `json:"attempts"`
}

type GatewayDLQItem struct {
	DeliveryID   string `json:"delivery_id"`
	RouteKey     string `json:"route_key"`
	Direction    string `json:"direction"`
	Status       string `json:"status"`
	ErrorClass   string `json:"error_class"`
	AttemptCount int    `json:"attempt_count"`
	ReplayCount  int    `json:"replay_count"`
	EnteredAt    string `json:"entered_at"`
	ActedAt      string `json:"acted_at,omitempty"`
}
