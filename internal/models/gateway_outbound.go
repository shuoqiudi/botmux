package models

// Gateway actions are deliberately narrow. A workload must be granted each
// action for each Business Route it uses.
const (
	GatewayActionMessagesSend    = "messages.send"
	GatewayActionCallbacksAnswer = "callbacks.answer"
	GatewayActionDeliveriesRead  = "deliveries.read"
)

// GatewayWorkload is the authenticated business caller. Credentials are kept
// in a separate table and are never represented by this type.
type GatewayWorkload struct {
	ID     int64
	Name   string
	Status string
}

// GatewayDelivery is the safe operational view of one durable item. It never
// contains the Telegram token, destination chat ID, callback ID, or payload.
type GatewayDelivery struct {
	ID             string `json:"delivery_id"`
	RouteID        int64  `json:"-"`
	RouteKey       string `json:"route_key"`
	RouteRevision  int64  `json:"route_revision"`
	WorkloadID     int64  `json:"-"`
	Direction      string `json:"direction"`
	Action         string `json:"action"`
	Status         string `json:"status"`
	AttemptCount   int    `json:"attempt_count"`
	SafeErrorClass string `json:"error_class,omitempty"`
	CreatedAt      string `json:"created_at"`
	AcceptedAt     string `json:"accepted_at,omitempty"`
	CompletedAt    string `json:"completed_at,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

// GatewayDeliveryCreate contains only logical identities and safe hashes. The
// payload is stored separately and is never selected by status APIs.
type GatewayDeliveryCreate struct {
	ID             string
	RouteID        int64
	RouteRevision  int64
	WorkloadID     int64
	Action         string
	PayloadHash    string
	IdempotencyKey string
	PayloadJSON    []byte
}

// GatewayOutboundTarget is used only inside the worker immediately before a
// Telegram call. Do not serialize or log this type.
type GatewayOutboundTarget struct {
	RouteKey string
	Enabled  bool
	Outbound bool
	Token    string
	ChatID   int64
}
