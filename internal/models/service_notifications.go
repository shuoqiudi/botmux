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
	ID     string `json:"delivery_id"`
	Status string `json:"status"`
}
