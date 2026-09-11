package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/skrashevich/botmux/internal/configbackup"
)

func configError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	message := "configuration storage unavailable"
	code := "storage_unavailable"
	switch {
	case errors.Is(err, configbackup.ErrInvalid):
		code = "invalid_snapshot"
		status = http.StatusBadRequest
		message = err.Error()
	case errors.Is(err, configbackup.ErrUnsupported):
		code = "unsupported_configuration"
		status = http.StatusUnprocessableEntity
		message = err.Error()
	case errors.Is(err, configbackup.ErrConflict):
		code = "target_conflict"
		status = http.StatusConflict
		message = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	result := map[string]string{"error": message, "code": code}
	var field *configbackup.FieldError
	if errors.As(err, &field) {
		result["location"] = field.Path
	}
	_ = json.NewEncoder(w).Encode(result)
}
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	snapshot, err := s.store.ExportConfiguration()
	if err != nil {
		configError(w, err)
		return
	}
	raw, _, err := snapshot.Canonical()
	if err != nil {
		configError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
func (s *Server) handleConfigRestore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		configError(w, configbackup.ErrInvalid)
		return
	}
	snapshot, err := configbackup.Decode(raw)
	if err != nil {
		configError(w, err)
		return
	}
	receipt, err := s.store.RestoreConfiguration(snapshot)
	if err != nil {
		configError(w, err)
		return
	}
	receipt.RuntimeLoaded = true
	ids, err := s.store.ConfigurationBotIDs()
	for _, b := range snapshot.Bots {
		id, found := ids[b.Ref]
		failed := err != nil || !found || s.proxy == nil
		if !failed && (!receipt.Replayed || (!b.Disabled && (b.ManageEnabled || b.ProxyEnabled) && !s.proxy.IsRunning(id))) {
			failed = s.proxy.RestartBot(id) != nil
		}
		if failed {
			receipt.RuntimeLoaded = false
			receipt.RuntimeFailedRefs = append(receipt.RuntimeFailedRefs, b.Ref)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(receipt)
}
