package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
)

func (s *Server) SetGatewayService(service *gateway.Service) {
	s.gateway = service
}

func (s *Server) SetGatewayOperations(operations *gateway.Operations) {
	s.gatewayOps = operations
}

func workloadCredential(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	credential := strings.TrimPrefix(header, "Bearer ")
	if credential == "" || strings.ContainsAny(credential, " \t\r\n") {
		return ""
	}
	return credential
}

func decodeStrictGatewayJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

func writeGatewayAPIError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := "the request could not be completed"
	switch {
	case errors.Is(err, gateway.ErrUnauthorized):
		status, code, message = http.StatusUnauthorized, "unauthorized", "valid workload authentication is required"
	case errors.Is(err, gateway.ErrForbidden):
		status, code, message = http.StatusForbidden, "forbidden", "workload is not permitted to perform this action"
	case errors.Is(err, gateway.ErrUnknownRoute):
		status, code, message = http.StatusNotFound, "unknown_route", "business route not found"
	case errors.Is(err, gateway.ErrRouteDisabled):
		status, code, message = http.StatusConflict, "route_disabled", "business route is not enabled for outbound delivery"
	case errors.Is(err, gateway.ErrIdempotencyRequired):
		status, code, message = http.StatusBadRequest, "idempotency_required", "a valid Idempotency-Key is required"
	case errors.Is(err, gateway.ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "idempotency_conflict", "idempotency key was already used with a different payload"
	case errors.Is(err, gateway.ErrInvalidPayload):
		status, code, message = http.StatusBadRequest, "invalid_payload", "request payload is invalid"
	case errors.Is(err, gateway.ErrQueueUnavailable):
		status, code, message = http.StatusServiceUnavailable, "queue_unavailable", "durable delivery queue is unavailable"
	case err != nil && err.Error() == "delivery_not_found":
		status, code, message = http.StatusNotFound, "delivery_not_found", "delivery not found"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

func (s *Server) gatewayWorkload(w http.ResponseWriter, r *http.Request) (*models.GatewayWorkload, bool) {
	if s.gateway == nil {
		writeGatewayAPIError(w, gateway.ErrQueueUnavailable)
		return nil, false
	}
	workload, err := s.gateway.Authenticate(r.Context(), workloadCredential(r))
	if err != nil {
		writeGatewayAPIError(w, err)
		return nil, false
	}
	return workload, true
}

func (s *Server) handleGatewayOutbound(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "routes" || !businessRouteKeyRE.MatchString(parts[3]) {
		writeGatewayAPIError(w, gateway.ErrUnknownRoute)
		return
	}
	workload, ok := s.gatewayWorkload(w, r)
	if !ok {
		return
	}
	routeKey := parts[3]
	switch {
	case len(parts) == 5 && parts[4] == "messages":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var input gateway.SendMessage
		if err := decodeStrictGatewayJSON(w, r, &input); err != nil {
			writeGatewayAPIError(w, gateway.ErrInvalidPayload)
			return
		}
		delivery, err := s.gateway.AcceptMessage(r.Context(), workload, routeKey, r.Header.Get("Idempotency-Key"), input)
		if err != nil {
			writeGatewayAPIError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"delivery_id": delivery.ID, "status": "accepted"})
	case len(parts) == 6 && parts[4] == "deliveries":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		delivery, err := s.gateway.GetDelivery(r.Context(), workload, routeKey, parts[5])
		if err != nil {
			writeGatewayAPIError(w, err)
			return
		}
		writeJSON(w, delivery)
	case len(parts) == 7 && parts[4] == "callbacks" && parts[6] == "answer":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var input struct {
			Text      string `json:"text"`
			ShowAlert bool   `json:"show_alert"`
		}
		if err := decodeStrictGatewayJSON(w, r, &input); err != nil {
			writeGatewayAPIError(w, gateway.ErrInvalidPayload)
			return
		}
		delivery, err := s.gateway.AcceptCallback(r.Context(), workload, routeKey, r.Header.Get("Idempotency-Key"), gateway.AnswerCallback{
			CallbackQueryID: parts[5], Text: input.Text, ShowAlert: input.ShowAlert,
		})
		if err != nil {
			writeGatewayAPIError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"delivery_id": delivery.ID, "status": "accepted"})
	default:
		writeGatewayAPIError(w, gateway.ErrUnknownRoute)
	}
}
