package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

var businessRouteKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type telegramEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

type telegramIdentity struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type telegramChat struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

func (s *Server) callTelegram(token, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(s.tgAPIURL(), "/") + "/bot" + url.PathEscape(token) + "/" + method
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("Telegram request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram is unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("Telegram response failed")
	}
	var env telegramEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if env.Description != "" {
			return fmt.Errorf("Telegram rejected the request: %s", env.Description)
		}
		return fmt.Errorf("Telegram rejected the request")
	}
	if result != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("Telegram returned an invalid response")
		}
	}
	return nil
}

func (s *Server) validateTelegramIdentity(token string) (telegramIdentity, error) {
	var identity telegramIdentity
	if strings.TrimSpace(token) == "" {
		return identity, errors.New("token is required")
	}
	if err := s.callTelegram(token, "getMe", map[string]any{}, &identity); err != nil {
		return identity, err
	}
	if identity.ID == 0 || identity.Username == "" {
		return identity, errors.New("Telegram bot identity is incomplete")
	}
	return identity, nil
}

func (s *Server) validateTelegramChat(token string, chatID int64) (telegramChat, error) {
	var chat telegramChat
	if chatID == 0 {
		return chat, errors.New("chat_id is required")
	}
	if err := s.callTelegram(token, "getChat", map[string]any{"chat_id": chatID}, &chat); err != nil {
		return chat, fmt.Errorf("bot cannot access destination: %w", err)
	}
	if chat.ID == 0 {
		return chat, errors.New("bot cannot access destination")
	}
	return chat, nil
}

func writeBusinessError(w http.ResponseWriter, status int, code string, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": err.Error()})
}

func itemID(path, _ string) int64 {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	value := parts[len(parts)-1]
	id, _ := strconv.ParseInt(value, 10, 64)
	return id
}

type botAccountInput struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

func (s *Server) handleBotAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		accounts, err := s.store.GetBotAccounts()
		if err != nil {
			writeBusinessError(w, 500, "storage_error", err)
			return
		}
		writeJSON(w, accounts)
	case http.MethodPost, http.MethodPut:
		var input botAccountInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
			writeBusinessError(w, 400, "invalid_request", errors.New("invalid JSON body"))
			return
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" {
			writeBusinessError(w, 400, "invalid_request", errors.New("name is required"))
			return
		}
		id := input.ID
		if id == 0 {
			id = itemID(r.URL.Path, "/api/bot-accounts/")
		}
		if r.Method == http.MethodPost && id == 0 {
			identity, err := s.validateTelegramIdentity(input.Token)
			if err != nil {
				writeBusinessError(w, 422, "telegram_validation_failed", err)
				return
			}
			id, err = s.store.AddBotAccount(models.BotAccount{Name: input.Name, Username: identity.Username, Token: input.Token})
			if err != nil {
				writeBusinessError(w, 409, "storage_error", err)
				return
			}
			account, _ := s.store.GetBotAccount(id)
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, account)
			return
		}
		if id == 0 {
			writeBusinessError(w, 400, "invalid_request", errors.New("id is required"))
			return
		}
		existing, err := s.store.GetBotAccount(id)
		if err != nil {
			writeBusinessError(w, 404, "not_found", errors.New("bot account not found"))
			return
		}
		username := existing.Username
		if input.Token != "" {
			identity, err := s.validateTelegramIdentity(input.Token)
			if err != nil {
				writeBusinessError(w, 422, "telegram_validation_failed", err)
				return
			}
			username = identity.Username
		}
		if err := s.store.UpdateBotAccount(models.BotAccount{ID: id, Name: input.Name, Username: username, Token: input.Token}); err != nil {
			writeBusinessError(w, 500, "storage_error", err)
			return
		}
		account, _ := s.store.GetBotAccount(id)
		writeJSON(w, account)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type destinationInput struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	BotAccountID int64  `json:"bot_account_id"`
	ChatID       int64  `json:"chat_id"`
}

func destinationTitle(chat telegramChat) string {
	if chat.Title != "" {
		return chat.Title
	}
	return strings.TrimSpace(chat.FirstName + " " + chat.LastName)
}

func (s *Server) validatedDestination(input destinationInput) (models.TelegramDestination, error) {
	if strings.TrimSpace(input.Name) == "" || input.BotAccountID == 0 || input.ChatID == 0 {
		return models.TelegramDestination{}, errors.New("name, bot_account_id and chat_id are required")
	}
	account, err := s.store.GetBotAccount(input.BotAccountID)
	if err != nil {
		return models.TelegramDestination{}, errors.New("bot account not found")
	}
	if _, err := s.validateTelegramIdentity(account.Token); err != nil {
		return models.TelegramDestination{}, err
	}
	chat, err := s.validateTelegramChat(account.Token, input.ChatID)
	if err != nil {
		return models.TelegramDestination{}, err
	}
	return models.TelegramDestination{ID: input.ID, Name: strings.TrimSpace(input.Name), BotAccountID: input.BotAccountID,
		ChatID: input.ChatID, ChatTitle: destinationTitle(chat), Status: "active", ValidatedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

func (s *Server) handleTelegramDestinations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		destinations, err := s.store.GetTelegramDestinations()
		if err != nil {
			writeBusinessError(w, 500, "storage_error", err)
			return
		}
		writeJSON(w, destinations)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input destinationInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
		writeBusinessError(w, 400, "invalid_request", errors.New("invalid JSON body"))
		return
	}
	if input.ID == 0 {
		input.ID = itemID(r.URL.Path, "/api/telegram-destinations/")
	}
	destination, err := s.validatedDestination(input)
	if err != nil {
		writeBusinessError(w, 422, "telegram_validation_failed", err)
		return
	}
	if r.Method == http.MethodPost && input.ID == 0 {
		id, err := s.store.AddTelegramDestination(destination)
		if err != nil {
			writeBusinessError(w, 409, "storage_error", err)
			return
		}
		stored, _ := s.store.GetTelegramDestination(id)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, stored)
		return
	}
	if input.ID == 0 {
		writeBusinessError(w, 400, "invalid_request", errors.New("id is required"))
		return
	}
	if _, err := s.store.GetTelegramDestination(input.ID); err != nil {
		writeBusinessError(w, 404, "not_found", errors.New("destination not found"))
		return
	}
	if err := s.store.UpdateTelegramDestination(destination); err != nil {
		writeBusinessError(w, 500, "storage_error", err)
		return
	}
	stored, _ := s.store.GetTelegramDestination(input.ID)
	writeJSON(w, stored)
}

type businessRouteInput struct {
	RouteKey          string   `json:"route_key"`
	DisplayName       string   `json:"display_name"`
	BotAccountID      int64    `json:"bot_account_id"`
	DestinationID     int64    `json:"destination_id"`
	InboundEnabled    bool     `json:"inbound_enabled"`
	InboundBackendURL string   `json:"inbound_backend_url"`
	OutboundEnabled   bool     `json:"outbound_enabled"`
	AllowedCallers    []string `json:"allowed_callers"`
	Enabled           *bool    `json:"enabled"`
}

func normalizeRouteInput(input businessRouteInput, creating bool) (models.BusinessRoute, error) {
	input.RouteKey = strings.TrimSpace(input.RouteKey)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if !businessRouteKeyRE.MatchString(input.RouteKey) {
		return models.BusinessRoute{}, errors.New("route_key must contain only lowercase letters, digits, '_' or '-'")
	}
	if input.DisplayName == "" || input.BotAccountID == 0 || input.DestinationID == 0 {
		return models.BusinessRoute{}, errors.New("display_name, bot_account_id and destination_id are required")
	}
	if input.InboundEnabled {
		u, err := url.ParseRequestURI(input.InboundBackendURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return models.BusinessRoute{}, errors.New("a valid inbound_backend_url is required when inbound is enabled")
		}
	}
	enabled := false
	if input.Enabled != nil {
		enabled = *input.Enabled
	} else if creating {
		enabled = true
	}
	return models.BusinessRoute{RouteKey: input.RouteKey, DisplayName: input.DisplayName, BotAccountID: input.BotAccountID,
		DestinationID: input.DestinationID, InboundEnabled: input.InboundEnabled, InboundBackendURL: strings.TrimSpace(input.InboundBackendURL),
		OutboundEnabled: input.OutboundEnabled, AllowedCallers: input.AllowedCallers, Enabled: enabled}, nil
}

func (s *Server) validateRoute(route *models.BusinessRoute) error {
	if err := s.store.ValidateBusinessRouteReferences(route.BotAccountID, route.DestinationID); err != nil {
		return err
	}
	if !route.Enabled {
		route.Status = "disabled"
		return nil
	}
	account, err := s.store.GetBotAccount(route.BotAccountID)
	if err != nil {
		return errors.New("bot account not found")
	}
	destination, err := s.store.GetTelegramDestination(route.DestinationID)
	if err != nil {
		return errors.New("destination not found")
	}
	if _, err := s.validateTelegramIdentity(account.Token); err != nil {
		return err
	}
	if _, err := s.validateTelegramChat(account.Token, destination.ChatID); err != nil {
		return err
	}
	route.Status = "active"
	route.LastValidatedAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}

func routeKeyFromPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 {
		return "", false
	}
	base := parts[len(parts)-1]
	if base == "routes" || base == "business-routes" {
		return "", false
	}
	if base == "test" && len(parts) >= 2 {
		return parts[len(parts)-2], true
	}
	return base, false
}

func (s *Server) handleBusinessRoutes(w http.ResponseWriter, r *http.Request) {
	key, isTest := routeKeyFromPath(r.URL.Path)
	if isTest {
		s.handleBusinessRouteTest(w, r, key)
		return
	}
	if r.Method == http.MethodGet {
		if key != "" {
			route, err := s.store.GetBusinessRoute(key)
			if err != nil {
				writeBusinessError(w, 404, "not_found", errors.New("business route not found"))
				return
			}
			writeJSON(w, route)
			return
		}
		routes, err := s.store.GetBusinessRoutes()
		if err != nil {
			writeBusinessError(w, 500, "storage_error", err)
			return
		}
		writeJSON(w, routes)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input businessRouteInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&input); err != nil {
		writeBusinessError(w, 400, "invalid_request", errors.New("invalid JSON body"))
		return
	}
	creating := r.Method == http.MethodPost && key == ""
	if key != "" {
		if input.RouteKey != "" && input.RouteKey != key {
			writeBusinessError(w, 409, "immutable_route_key", store.ErrRouteKeyImmutable)
			return
		}
		input.RouteKey = key
	}
	route, err := normalizeRouteInput(input, creating)
	if err != nil {
		writeBusinessError(w, 400, "invalid_request", err)
		return
	}
	if err := s.validateRoute(&route); err != nil {
		writeBusinessError(w, 422, "telegram_validation_failed", err)
		return
	}
	if creating {
		if _, err := s.store.AddBusinessRoute(route); err != nil {
			writeBusinessError(w, 409, "storage_error", err)
			return
		}
		stored, _ := s.store.GetBusinessRoute(route.RouteKey)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, stored)
		return
	}
	if key == "" {
		key = route.RouteKey
	}
	if _, err := s.store.GetBusinessRoute(key); err != nil {
		writeBusinessError(w, 404, "not_found", errors.New("business route not found"))
		return
	}
	if err := s.store.UpdateBusinessRoute(key, route); err != nil {
		if errors.Is(err, store.ErrRouteKeyImmutable) {
			writeBusinessError(w, 409, "immutable_route_key", err)
			return
		}
		writeBusinessError(w, 409, "storage_error", err)
		return
	}
	stored, _ := s.store.GetBusinessRoute(key)
	writeJSON(w, stored)
}

type businessRouteSetupInput struct {
	BotAccount struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	} `json:"bot_account"`
	Destination struct {
		Name   string `json:"name"`
		ChatID int64  `json:"chat_id"`
	} `json:"destination"`
	Route businessRouteInput `json:"route"`
}

func (s *Server) handleBusinessRouteSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input businessRouteSetupInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&input); err != nil {
		writeBusinessError(w, 400, "invalid_request", errors.New("invalid JSON body"))
		return
	}
	if strings.TrimSpace(input.BotAccount.Name) == "" || strings.TrimSpace(input.Destination.Name) == "" {
		writeBusinessError(w, 400, "invalid_request", errors.New("bot account and destination names are required"))
		return
	}
	identity, err := s.validateTelegramIdentity(input.BotAccount.Token)
	if err != nil {
		writeBusinessError(w, 422, "telegram_validation_failed", err)
		return
	}
	chat, err := s.validateTelegramChat(input.BotAccount.Token, input.Destination.ChatID)
	if err != nil {
		writeBusinessError(w, 422, "telegram_validation_failed", err)
		return
	}
	// Placeholder references allow shared request validation; the transaction
	// replaces them with the newly created stable IDs.
	input.Route.BotAccountID, input.Route.DestinationID = 1, 1
	route, err := normalizeRouteInput(input.Route, true)
	if err != nil {
		writeBusinessError(w, 400, "invalid_request", err)
		return
	}
	if route.Enabled {
		route.Status = "active"
		route.LastValidatedAt = time.Now().UTC().Format(time.RFC3339)
	} else {
		route.Status = "disabled"
	}
	stored, err := s.store.CreateBusinessRouteSetup(
		models.BotAccount{Name: strings.TrimSpace(input.BotAccount.Name), Username: identity.Username, Token: input.BotAccount.Token},
		models.TelegramDestination{Name: strings.TrimSpace(input.Destination.Name), ChatID: input.Destination.ChatID, ChatTitle: destinationTitle(chat), ValidatedAt: route.LastValidatedAt}, route)
	if err != nil {
		writeBusinessError(w, 409, "storage_error", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, stored)
}

func (s *Server) handleBusinessRouteTest(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil || strings.TrimSpace(input.Text) == "" {
		writeBusinessError(w, 400, "invalid_request", errors.New("text is required"))
		return
	}
	route, err := s.store.GetBusinessRoute(key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeBusinessError(w, 404, "not_found", errors.New("business route not found"))
		} else {
			writeBusinessError(w, 500, "storage_error", err)
		}
		return
	}
	if !route.Enabled || route.Status != "active" || !route.OutboundEnabled {
		writeBusinessError(w, 409, "route_not_sendable", errors.New("business route is not active for outbound messages"))
		return
	}
	account, err := s.store.GetBotAccount(route.BotAccountID)
	if err != nil {
		writeBusinessError(w, 500, "storage_error", err)
		return
	}
	destination, err := s.store.GetTelegramDestination(route.DestinationID)
	if err != nil {
		writeBusinessError(w, 500, "storage_error", err)
		return
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	if err := s.callTelegram(account.Token, "sendMessage", map[string]any{"chat_id": destination.ChatID, "text": input.Text}, &sent); err != nil {
		writeBusinessError(w, 502, "telegram_send_failed", err)
		return
	}
	writeJSON(w, map[string]any{"status": "sent", "route_key": route.RouteKey, "message_id": sent.MessageID})
}
