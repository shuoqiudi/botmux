package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

func (s *Server) handleServiceImport(w http.ResponseWriter, r *http.Request) {
	if getAuthUser(r).Role != "admin" {
		writeServiceError(w, 403, "forbidden")
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var input models.ServiceImport
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeServiceError(w, 400, "invalid_request")
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeServiceError(w, 400, "invalid_request")
		return
	}
	if !validServiceLabel(input.MigrationID, 128) || len(input.Services) == 0 || len(input.Services) > 100 {
		writeServiceError(w, 400, "invalid_manifest")
		return
	}
	seen := map[int64]map[string]bool{}
	for _, entry := range input.Services {
		if entry.WorkloadID <= 0 || !validServiceLabel(entry.Fingerprint, 512) || !validServiceLabel(entry.DisplayName, 256) || len(entry.Destinations) == 0 || len(entry.Destinations) > 100 {
			writeServiceError(w, 400, "invalid_manifest")
			return
		}
		if seen[entry.WorkloadID] == nil {
			seen[entry.WorkloadID] = map[string]bool{}
		}
		if seen[entry.WorkloadID][entry.Fingerprint] {
			writeServiceError(w, 400, "duplicate_service")
			return
		}
		seen[entry.WorkloadID][entry.Fingerprint] = true
		destinations := map[int64]bool{}
		for _, d := range entry.Destinations {
			if d.DestinationID <= 0 || d.BotAccountID <= 0 || d.TelegramBotID <= 0 || d.ChatID == 0 || destinations[d.DestinationID] {
				writeServiceError(w, 400, "invalid_destination")
				return
			}
			destinations[d.DestinationID] = true
		}
	}
	ids, err := s.store.ImportNotificationServices(r.Context(), input, gatewayAdminActor(r))
	switch {
	case errors.Is(err, store.ErrServiceImportConflict), errors.Is(err, store.ErrDuplicateSubscription), errors.Is(err, store.ErrInvalidSubscriptionTarget):
		writeServiceError(w, 409, "import_conflict")
	case errors.Is(err, sql.ErrNoRows):
		writeServiceError(w, 400, "unknown_source_or_destination")
	case err != nil:
		writeServiceError(w, 503, "storage_unavailable")
	default:
		writeJSON(w, map[string]any{"migration_id": input.MigrationID, "service_ids": ids})
	}
}
