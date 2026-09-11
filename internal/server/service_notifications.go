package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

// encoding/json replaces unpaired UTF-16 surrogates with U+FFFD. Reject
// them so malformed producer identities cannot silently collapse together.
func validJSONStringUnicode(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xDC00 && code <= 0xDFFF {
			return false
		}
		if code >= 0xD800 && code <= 0xDBFF {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

func decodeServiceNotification(w http.ResponseWriter, r *http.Request) (models.ServiceNotificationRequest, error) {
	var input models.ServiceNotificationRequest
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return input, gateway.ErrInvalidPayload
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || !utf8.Valid(raw) {
		return input, gateway.ErrInvalidPayload
	}
	// Decode each member explicitly: null, duplicate and unknown fields are not
	// producer fields, even when encoding/json would otherwise accept them.
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return input, gateway.ErrInvalidPayload
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return input, gateway.ErrInvalidPayload
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return input, gateway.ErrInvalidPayload
		}
		seen[name] = true
		var encoded json.RawMessage
		if err := decoder.Decode(&encoded); err != nil || !validJSONStringUnicode(encoded) {
			return input, gateway.ErrInvalidPayload
		}
		var value *string
		if err := json.Unmarshal(encoded, &value); err != nil || value == nil {
			return input, gateway.ErrInvalidPayload
		}
		switch name {
		case "fingerprint":
			input.Fingerprint = *value
		case "text":
			input.Text = *value
		case "service_name":
			input.ServiceName = value
		case "parse_mode":
			input.ParseMode = *value
		default:
			return input, gateway.ErrInvalidPayload
		}
	}
	if _, err := decoder.Token(); err != nil {
		return input, gateway.ErrInvalidPayload
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return input, gateway.ErrInvalidPayload
	}
	if !models.ValidServiceLabel(input.Fingerprint, 512) || (input.ServiceName != nil && !models.ValidServiceLabel(*input.ServiceName, 256)) {
		return input, gateway.ErrInvalidPayload
	}
	if err := gateway.ValidateSendMessage(gateway.SendMessage{Text: input.Text, ParseMode: input.ParseMode}); err != nil {
		return input, gateway.ErrInvalidPayload
	}
	return input, nil
}

func writeServiceError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func serviceNotificationReceipt(n models.ServiceNotification) models.ServiceNotification {
	n.Text = ""
	n.ParseMode = ""
	n.Deliveries = append([]models.ServiceNotificationDelivery{}, n.Deliveries...)
	for i := range n.Deliveries {
		d := &n.Deliveries[i]
		d.SubscriptionID, d.BotAccountID, d.TelegramBotID, d.ChatID = 0, 0, 0, 0
	}
	return n
}

func (s *Server) handleServiceNotifications(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/services/notifications"
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	publish := suffix == ""
	if !publish && (len(suffix) < 2 || strings.Contains(suffix[1:], "/")) {
		writeServiceError(w, 404, "notification_not_found")
		return
	}
	if (publish && r.Method != http.MethodPost) || (!publish && r.Method != http.MethodGet) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	credential := workloadCredential(r)
	if credential == "" {
		writeGatewayAPIError(w, gateway.ErrUnauthorized)
		return
	}
	workload, err := s.store.AuthenticateGatewayWorkload(r.Context(), auth.HashAPIKey(credential))
	if errors.Is(err, sql.ErrNoRows) {
		writeGatewayAPIError(w, gateway.ErrUnauthorized)
		return
	}
	if err != nil {
		writeServiceError(w, 503, "storage_unavailable")
		return
	}
	permissions, err := s.store.GetServicePermissions(r.Context(), workload.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeGatewayAPIError(w, gateway.ErrUnauthorized)
		return
	}
	if err != nil {
		writeServiceError(w, 503, "storage_unavailable")
		return
	}
	if (publish && !permissions.Publish) || (!publish && !permissions.Query) {
		writeGatewayAPIError(w, gateway.ErrForbidden)
		return
	}
	if !publish {
		n, err := s.store.GetServiceNotification(r.Context(), workload.ID, suffix[1:])
		if errors.Is(err, sql.ErrNoRows) {
			writeServiceError(w, 404, "notification_not_found")
			return
		}
		if err != nil {
			writeServiceError(w, 503, "storage_unavailable")
			return
		}
		writeJSON(w, serviceNotificationReceipt(*n))
		return
	}
	if err := gateway.ValidateIdempotencyKey(r.Header.Get("Idempotency-Key")); err != nil {
		writeGatewayAPIError(w, err)
		return
	}
	input, err := decodeServiceNotification(w, r)
	if err != nil {
		writeGatewayAPIError(w, err)
		return
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		writeGatewayAPIError(w, gateway.ErrInvalidPayload)
		return
	}
	hash := sha256.Sum256(canonical)
	n, err := s.store.AcceptServiceNotification(r.Context(), workload.ID, r.Header.Get("Idempotency-Key"), hex.EncodeToString(hash[:]), input)
	if errors.Is(err, store.ErrGatewayIdempotencyConflict) {
		writeGatewayAPIError(w, gateway.ErrIdempotencyConflict)
		return
	}
	if err != nil {
		writeServiceError(w, 503, "storage_unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(serviceNotificationReceipt(*n))
}

func (s *Server) handleNotificationServicesAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/gateway/v1/services/import" {
		s.handleServiceImport(w, r)
		return
	}
	if strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/gateway/v1/services"), "/subscriptions") {
		s.handleServiceSubscriptions(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/api/gateway/v1/services"
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	if suffix == "" {
		items, err := s.store.ListNotificationServices(r.Context())
		if err != nil {
			writeServiceError(w, 503, "storage_unavailable")
			return
		}
		writeJSON(w, items)
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(suffix, "/"), 10, 64)
	if err != nil || id <= 0 {
		writeServiceError(w, 404, "service_not_found")
		return
	}
	service, err := s.store.GetNotificationService(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeServiceError(w, 404, "service_not_found")
		return
	}
	if err != nil {
		writeServiceError(w, 503, "storage_unavailable")
		return
	}
	notifications, err := s.store.ServiceNotificationHistory(r.Context(), id, r.URL.Query().Get("before"))
	if err != nil {
		writeServiceError(w, 503, "storage_unavailable")
		return
	}
	if getAuthUser(r).Role != "admin" {
		for i := range notifications {
			notifications[i] = serviceNotificationReceipt(notifications[i])
		}
	}
	next := ""
	if len(notifications) == 100 {
		next = notifications[len(notifications)-1].ID
	}
	subscriptions, err := s.store.ListServiceSubscriptions(r.Context(), id)
	if err != nil {
		writeServiceError(w, 503, "storage_unavailable")
		return
	}
	writeJSON(w, map[string]any{"service": service, "subscriptions": subscriptions, "notifications": notifications, "next_before": next})
}

func (s *Server) handleServiceSubscriptions(w http.ResponseWriter, r *http.Request) {
	if getAuthUser(r).Role != "admin" {
		writeServiceError(w, 403, "forbidden")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/gateway/v1/services/"), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "subscriptions" {
		writeServiceError(w, 404, "not_found")
		return
	}
	serviceID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || serviceID <= 0 {
		writeServiceError(w, 404, "service_not_found")
		return
	}
	if (r.Method != http.MethodPost || len(parts) != 2) && (r.Method != http.MethodDelete || len(parts) != 3) {
		w.WriteHeader(405)
		return
	}
	var input struct {
		ExpectedRevision int64 `json:"expected_revision"`
		DestinationID    int64 `json:"destination_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeServiceError(w, 400, "invalid_request")
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeServiceError(w, 400, "invalid_request")
		return
	}
	if input.ExpectedRevision <= 0 {
		writeServiceError(w, 400, "expected_revision_required")
		return
	}
	var id int64
	if r.Method == http.MethodPost {
		id, err = s.store.AddServiceSubscription(r.Context(), serviceID, input.DestinationID, input.ExpectedRevision, gatewayAdminActor(r))
	} else {
		id, err = strconv.ParseInt(parts[2], 10, 64)
		if err != nil || id <= 0 {
			writeServiceError(w, 404, "subscription_not_found")
			return
		}
		err = s.store.CancelServiceSubscription(r.Context(), serviceID, id, input.ExpectedRevision, gatewayAdminActor(r))
	}
	switch {
	case errors.Is(err, store.ErrRevisionConflict):
		writeServiceError(w, 409, "revision_conflict")
		return
	case errors.Is(err, store.ErrDuplicateSubscription):
		writeServiceError(w, 409, "duplicate_subscription")
		return
	case errors.Is(err, store.ErrInvalidSubscriptionTarget):
		writeServiceError(w, 400, "invalid_subscription_target")
		return
	case errors.Is(err, sql.ErrNoRows):
		writeServiceError(w, 404, "not_found")
		return
	case err != nil:
		writeServiceError(w, 503, "storage_unavailable")
		return
	}
	if r.Method == http.MethodPost {
		w.WriteHeader(201)
	}
	writeJSON(w, map[string]any{"id": id, "revision": input.ExpectedRevision + 1})
}
