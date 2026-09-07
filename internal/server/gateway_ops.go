package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/skrashevich/botmux/internal/gateway"
)

func (s *Server) requireGatewayOps(w http.ResponseWriter) bool {
	if s.gatewayOps == nil {
		writeBusinessError(w, http.StatusServiceUnavailable, "operations_unavailable", errors.New("Gateway operations are not configured"))
		return false
	}
	return true
}

func (s *Server) handleGatewayOpsHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireGatewayOps(w) {
		return
	}
	writeJSON(w, s.gatewayOps.Health(r.Context()))
}

func (s *Server) handleGatewayRouteOps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.requireGatewayOps(w) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/gateway/v1/ops/routes/"), "/")
	if len(parts) == 0 || !businessRouteKeyRE.MatchString(parts[0]) {
		writeBusinessError(w, http.StatusNotFound, "not_found", errors.New("Business Route not found"))
		return
	}
	routeKey := parts[0]
	if len(parts) == 1 {
		metrics, err := s.gatewayOps.RouteMetrics(r.Context(), routeKey)
		if errors.Is(err, sql.ErrNoRows) {
			writeBusinessError(w, http.StatusNotFound, "not_found", errors.New("Business Route not found"))
			return
		}
		if err != nil {
			writeBusinessError(w, http.StatusInternalServerError, "operations_error", errors.New("Gateway route status is unavailable"))
			return
		}
		if metrics.NativeBotID == 0 {
			metrics.Components.TelegramPolling.Status = "unavailable"
			metrics.Components.TelegramPolling.Detail = "polling_bot_not_configured"
		} else if s.proxy != nil && s.proxy.IsRunning(metrics.NativeBotID) {
			metrics.Components.TelegramPolling.Status = "healthy"
		} else {
			metrics.Components.TelegramPolling.Status = "unhealthy"
			metrics.Components.TelegramPolling.Detail = "poller_stopped"
		}
		writeJSON(w, metrics)
		return
	}
	if len(parts) == 3 && parts[1] == "deliveries" && parts[2] != "" {
		detail, err := s.gatewayOps.DeliveryDetail(r.Context(), routeKey, parts[2])
		if errors.Is(err, sql.ErrNoRows) {
			writeBusinessError(w, http.StatusNotFound, "not_found", errors.New("Delivery not found"))
			return
		}
		if err != nil {
			writeBusinessError(w, http.StatusInternalServerError, "operations_error", errors.New("Delivery status is unavailable"))
			return
		}
		writeJSON(w, detail)
		return
	}
	if len(parts) == 2 && parts[1] == "dlq" {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := s.gatewayOps.ListDLQ(r.Context(), routeKey, r.URL.Query().Get("state"), limit)
		if err != nil {
			writeBusinessError(w, http.StatusInternalServerError, "operations_error", errors.New("DLQ is unavailable"))
			return
		}
		writeJSON(w, items)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) handleGatewayOpsDLQ(w http.ResponseWriter, r *http.Request) {
	if !s.requireGatewayOps(w) {
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/api/gateway/v1/ops/dlq")
	if suffix == "" || suffix == "/" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items, err := s.gatewayOps.ListDLQ(r.Context(), r.URL.Query().Get("route_key"), r.URL.Query().Get("state"), limit)
		if err != nil {
			writeBusinessError(w, http.StatusInternalServerError, "operations_error", errors.New("DLQ is unavailable"))
			return
		}
		writeJSON(w, items)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(suffix, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	actor := gatewayAdminActor(r)
	var item any
	var unchanged bool
	var err error
	switch parts[1] {
	case "replay":
		item, unchanged, err = s.gatewayOps.ReplayDLQ(r.Context(), parts[0], actor)
	case "discard":
		item, unchanged, err = s.gatewayOps.DiscardDLQ(r.Context(), parts[0], actor)
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if gateway.IsDLQNotFound(err) {
		writeBusinessError(w, http.StatusNotFound, "not_found", errors.New("DLQ item not found"))
		return
	}
	if gateway.IsDLQConflict(err) {
		writeBusinessError(w, http.StatusConflict, "invalid_dlq_state", errors.New("DLQ item cannot transition from its current state"))
		return
	}
	if errors.Is(err, gateway.ErrQueueUnavailable) {
		writeBusinessError(w, http.StatusServiceUnavailable, "queue_unavailable", errors.New("Durable queue is unavailable"))
		return
	}
	if err != nil {
		writeBusinessError(w, http.StatusInternalServerError, "operations_error", errors.New("DLQ operation failed"))
		return
	}
	writeJSON(w, map[string]any{"item": item, "idempotent": unchanged})
}
