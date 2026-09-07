package models

// BotAccount is the stable Telegram identity used by Business Routes. Token is
// deliberately write-only at JSON boundaries.
type BotAccount struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Username        string `json:"username"`
	Token           string `json:"-"`
	TokenSet        bool   `json:"token_set"`
	TokenConfigured bool   `json:"token_configured"`
	Revision        int64  `json:"revision"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	RuntimeStatus   string `json:"runtime_status,omitempty"`
}

// TelegramDestination names a chat that is reachable by one Bot Account.
type TelegramDestination struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	BotAccountID int64  `json:"bot_account_id"`
	ChatID       int64  `json:"chat_id"`
	ChatTitle    string `json:"chat_title"`
	Status       string `json:"status"`
	ValidatedAt  string `json:"validated_at,omitempty"`
	Revision     int64  `json:"revision"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// BusinessRoute binds the immutable business path to mutable Telegram and
// inbound/outbound configuration.
type BusinessRoute struct {
	ID                      int64    `json:"id"`
	RouteKey                string   `json:"route_key"`
	DisplayName             string   `json:"display_name"`
	BotAccountID            int64    `json:"bot_account_id"`
	DestinationID           int64    `json:"destination_id"`
	InboundEnabled          bool     `json:"inbound_enabled"`
	InboundBackendURL       string   `json:"inbound_backend_url,omitempty"`
	InboundBackendHealthURL string   `json:"inbound_backend_health_url,omitempty"`
	InboundBackendToken     string   `json:"-"`
	BackendCredentialSet    bool     `json:"backend_credential_set"`
	CredentialConfigured    bool     `json:"credential_configured"`
	OutboundEnabled         bool     `json:"outbound_enabled"`
	AllowedCallers          []string `json:"allowed_callers"`
	Enabled                 bool     `json:"enabled"`
	Revision                int64    `json:"revision"`
	Status                  string   `json:"status"`
	LastValidatedAt         string   `json:"last_validated_at,omitempty"`
	CreatedAt               string   `json:"created_at"`
	UpdatedAt               string   `json:"updated_at"`
	BotAccountName          string   `json:"bot_account_name,omitempty"`
	BotUsername             string   `json:"bot_username,omitempty"`
	DestinationName         string   `json:"destination_name,omitempty"`
	DestinationChatID       int64    `json:"destination_chat_id,omitempty"`
	Path                    string   `json:"path"`
}

// GatewayRouteProbeTarget is private runtime material for component probes.
// It must never be serialized, logged, or returned from an HTTP handler.
type GatewayRouteProbeTarget struct {
	Token            string `json:"-"`
	ChatID           int64  `json:"-"`
	BackendHealthURL string `json:"-"`
	BackendToken     string `json:"-"`
}
