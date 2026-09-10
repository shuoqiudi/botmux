package models

// ServiceNotificationRequest is the producer contract, independent of Telegram targets.
type ServiceNotificationRequest struct {
	Fingerprint string  `json:"fingerprint"`
	Text        string  `json:"text"`
	ServiceName *string `json:"service_name,omitempty"`
	ParseMode   string  `json:"parse_mode,omitempty"`
}

type ServicePermissions struct {
	Publish bool `json:"publish"`
	Query   bool `json:"query"`
}

type NotificationService struct {
	ID                int64  `json:"id"`
	Revision          int64  `json:"revision"`
	WorkloadID        int64  `json:"workload_id"`
	Source            string `json:"source"`
	Fingerprint       string `json:"fingerprint"`
	DisplayName       string `json:"display_name"`
	SubscriptionCount int    `json:"subscription_count"`
	LastReceivedAt    string `json:"last_received_at"`
}

type ServiceNotification struct {
	ID                string                        `json:"notification_id"`
	ServiceID         int64                         `json:"service_id"`
	SubscriptionCount int                           `json:"subscription_count"`
	Status            string                        `json:"status"`
	ReceivedAt        string                        `json:"received_at"`
	DeliverySummary   ServiceDeliverySummary        `json:"delivery_summary"`
	Deliveries        []ServiceNotificationDelivery `json:"deliveries"`
	Text              string                        `json:"text,omitempty"`
	ParseMode         string                        `json:"parse_mode,omitempty"`
}

type ServiceDeliverySummary struct {
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Succeeded int    `json:"succeeded"`
}

type ServiceNotificationDelivery struct {
	ID           string `json:"delivery_id"`
	Status       string `json:"status"`
	ErrorClass   string `json:"error_class,omitempty"`
	AttemptCount int    `json:"attempt_count"`
}

// ServiceSubscription references a managed destination; credentials stay on its account.
type ServiceSubscription struct {
	ID              int64  `json:"id"`
	DestinationID   int64  `json:"destination_id"`
	BotAccountID    int64  `json:"bot_account_id"`
	BotName         string `json:"bot_name"`
	DestinationName string `json:"destination_name"`
	ChatTitle       string `json:"chat_title"`
	Active          bool   `json:"active"`
}

// ServiceEnqueue records durable queue work. Generation zero is the original
// append; positive generations identify an operator-requested DLQ replay.
type ServiceEnqueue struct {
	DeliveryID       string
	ReplayGeneration int
}
