package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/skrashevich/botmux/internal/auth"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

func gatewaySecurityID(path string) (int64, string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 5 {
		return 0, ""
	}
	id, _ := strconv.ParseInt(parts[4], 10, 64)
	action := ""
	if len(parts) > 5 {
		action = strings.Join(parts[5:], "/")
	}
	return id, action
}

func (s *Server) handleGatewayWorkloads(w http.ResponseWriter, r *http.Request) {
	id, action := gatewaySecurityID(r.URL.Path)
	if id < 0 {
		writeBusinessError(w, 404, "not_found", sql.ErrNoRows)
		return
	}
	if id == 0 && r.Method == http.MethodGet {
		items, err := s.store.GetGatewayWorkloadsAdmin()
		if err != nil {
			writeBusinessError(w, 500, "storage_error", err)
			return
		}
		writeJSON(w, items)
		return
	}
	if id == 0 && r.Method == http.MethodPost {
		var input struct {
			Name           string `json:"name"`
			CredentialName string `json:"credential_name"`
		}
		if err := decodeStrictGatewayJSON(w, r, &input); err != nil || strings.TrimSpace(input.Name) == "" {
			writeBusinessError(w, 400, "invalid_request", errors.New("name is required"))
			return
		}
		if input.CredentialName == "" {
			input.CredentialName = "initial"
		}
		random, err := auth.GenerateSessionToken()
		if err != nil {
			writeBusinessError(w, 500, "credential_generation_failed", err)
			return
		}
		credential := "gwk_" + random
		item, err := s.store.CreateGatewayWorkloadAdmin(strings.TrimSpace(input.Name), strings.TrimSpace(input.CredentialName), auth.HashAPIKey(credential), gatewayAdminActor(r))
		if err != nil {
			writeBusinessError(w, 409, "storage_error", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workload": item, "credential": credential})
		return
	}
	if id == 0 {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodGet && action == "" {
		item, err := s.store.GetGatewayWorkloadAdmin(id)
		if err != nil {
			writeBusinessError(w, 404, "not_found", sql.ErrNoRows)
			return
		}
		writeJSON(w, item)
		return
	}
	if r.Method == http.MethodPut && action == "" {
		var input struct {
			ExpectedRevision int64  `json:"expected_revision"`
			Status           string `json:"status"`
		}
		if err := decodeStrictGatewayJSON(w, r, &input); err != nil {
			writeBusinessError(w, 400, "invalid_request", err)
			return
		}
		item, err := s.store.SetGatewayWorkloadStatus(id, input.ExpectedRevision, input.Status, gatewayAdminActor(r))
		if gatewaySecurityError(w, err) {
			return
		}
		writeJSON(w, item)
		return
	}
	if r.Method == http.MethodPost && action == "credentials/rotate" {
		var input struct {
			ExpectedRevision int64  `json:"expected_revision"`
			Name             string `json:"name"`
		}
		if err := decodeStrictGatewayJSON(w, r, &input); err != nil {
			writeBusinessError(w, 400, "invalid_request", err)
			return
		}
		if input.Name == "" {
			input.Name = "rotated"
		}
		random, err := auth.GenerateSessionToken()
		if err != nil {
			writeBusinessError(w, 500, "credential_generation_failed", err)
			return
		}
		credential := "gwk_" + random
		item, err := s.store.RotateGatewayWorkloadCredential(id, input.ExpectedRevision, input.Name, auth.HashAPIKey(credential), gatewayAdminActor(r))
		if gatewaySecurityError(w, err) {
			return
		}
		writeJSON(w, map[string]any{"workload": item, "credential": credential})
		return
	}
	if r.Method == http.MethodPut && action == "service-permissions" {
		var input struct {
			ExpectedRevision int64 `json:"expected_revision"`
			Publish          bool  `json:"publish"`
			Query            bool  `json:"query"`
		}
		if err := decodeStrictGatewayJSON(w, r, &input); err != nil {
			writeBusinessError(w, 400, "invalid_request", errors.New("invalid request"))
			return
		}
		item, err := s.store.SetServicePermissions(id, input.ExpectedRevision, models.ServicePermissions{Publish: input.Publish, Query: input.Query}, gatewayAdminActor(r))
		if gatewaySecurityError(w, err) {
			return
		}
		writeJSON(w, item)
		return
	}
	if r.Method == http.MethodPut && action == "permissions" {
		var input struct {
			ExpectedRevision int64                      `json:"expected_revision"`
			Permissions      []models.GatewayPermission `json:"permissions"`
		}
		if err := decodeStrictGatewayJSON(w, r, &input); err != nil {
			writeBusinessError(w, 400, "invalid_request", err)
			return
		}
		item, err := s.store.SetGatewayPermissions(id, input.ExpectedRevision, input.Permissions, gatewayAdminActor(r))
		if gatewaySecurityError(w, err) {
			return
		}
		writeJSON(w, item)
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func gatewaySecurityError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, store.ErrExpectedRevisionRequired):
		writeBusinessError(w, 428, "expected_revision_required", err)
	case errors.Is(err, store.ErrRevisionConflict):
		writeBusinessError(w, 409, "revision_conflict", err)
	case errors.Is(err, sql.ErrNoRows):
		writeBusinessError(w, 404, "not_found", err)
	default:
		writeBusinessError(w, 400, "invalid_request", err)
	}
	return true
}

func (s *Server) handleGatewayAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.GetGatewayAudit(r.URL.Query().Get("route_key"), limit)
	if err != nil {
		writeBusinessError(w, 500, "storage_error", err)
		return
	}
	writeJSON(w, events)
}
