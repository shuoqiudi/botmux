package models

type InboundDeliveryStatus string

const (
	InboundPending               InboundDeliveryStatus = "PENDING"
	InboundRetrying              InboundDeliveryStatus = "RETRYING"
	InboundSucceeded             InboundDeliveryStatus = "SUCCEEDED"
	InboundDLQ                   InboundDeliveryStatus = "DLQ"
	InboundRejectedUnknownRoute  InboundDeliveryStatus = "REJECTED_UNKNOWN_ROUTE"
	InboundRejectedRouteDisabled InboundDeliveryStatus = "REJECTED_ROUTE_DISABLED"
)

// InboundRoute is the private, revision-pinned backend resolution used while
// accepting a Telegram Update. Credentials are never serialized.
type InboundRoute struct {
	ID             int64
	Revision       int64
	RouteKey       string
	BotAccountID   int64
	Enabled        bool
	InboundEnabled bool
	Status         GatewayRouteStatus
	BackendURL     string
	BackendToken   string
}

// InboundDelivery is the durable record for one Telegram Update. RawUpdate
// and BackendToken intentionally have no JSON representation.
type InboundDelivery struct {
	DeliveryID      string                `json:"delivery_id"`
	BotID           int64                 `json:"-"`
	BotAccountID    int64                 `json:"-"`
	RouteID         int64                 `json:"route_id,omitempty"`
	RouteRevision   int64                 `json:"route_revision,omitempty"`
	RouteKey        string                `json:"route_key,omitempty"`
	UpdateID        int64                 `json:"update_id"`
	CallbackQueryID string                `json:"callback_query_id,omitempty"`
	Status          InboundDeliveryStatus `json:"status"`
	StreamID        string                `json:"-"`
	AttemptCount    int                   `json:"attempt_count"`
	NextAttemptAt   string                `json:"next_attempt_at,omitempty"`
	LastErrorClass  string                `json:"last_error_class,omitempty"`
	RawUpdate       []byte                `json:"-"`
	BackendURL      string                `json:"-"`
	BackendToken    string                `json:"-"`
	CreatedAt       string                `json:"created_at"`
	UpdatedAt       string                `json:"updated_at"`
}
