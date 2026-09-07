package models

type GatewayWorkloadCredential struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	RotatedAt  string `json:"rotated_at,omitempty"`
}

type GatewayPermission struct {
	RouteKey string        `json:"route_key"`
	Action   GatewayAction `json:"action"`
}

type GatewayWorkloadAdmin struct {
	ID          int64                       `json:"id"`
	Name        string                      `json:"name"`
	Status      string                      `json:"status"`
	Revision    int64                       `json:"revision"`
	Credentials []GatewayWorkloadCredential `json:"credentials"`
	Permissions []GatewayPermission         `json:"permissions"`
	CreatedAt   string                      `json:"created_at"`
	UpdatedAt   string                      `json:"updated_at"`
}

type GatewayAuditEvent struct {
	ID        int64          `json:"id"`
	RouteID   int64          `json:"route_id,omitempty"`
	RouteKey  string         `json:"route_key,omitempty"`
	Revision  int64          `json:"revision"`
	ActorKind string         `json:"actor_kind"`
	ActorID   string         `json:"actor_id"`
	Action    string         `json:"action"`
	SafeDiff  map[string]any `json:"diff"`
	CreatedAt string         `json:"created_at"`
}
