package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type TelegramError struct {
	Class     string
	Retryable bool
}

func (e *TelegramError) Error() string { return e.Class }

type TelegramHTTPClient struct {
	baseURL string
	client  *http.Client
}

func NewTelegramHTTPClient(baseURL string) *TelegramHTTPClient {
	return &TelegramHTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

type telegramResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
}

func (c *TelegramHTTPClient) call(ctx context.Context, token, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := c.baseURL + "/bot" + url.PathEscape(token) + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &TelegramError{Class: "telegram_unavailable", Retryable: true}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return &TelegramError{Class: "telegram_unavailable", Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &TelegramError{Class: "telegram_unavailable", Retryable: true}
	}
	var envelope telegramResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return &TelegramError{Class: "telegram_invalid_response", Retryable: resp.StatusCode >= 500}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &TelegramError{Class: "telegram_rate_limited", Retryable: true}
	}
	if resp.StatusCode >= 500 {
		return &TelegramError{Class: "telegram_unavailable", Retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.OK {
		return &TelegramError{Class: "telegram_rejected", Retryable: false}
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return &TelegramError{Class: "telegram_invalid_response", Retryable: false}
		}
	}
	return nil
}

func (c *TelegramHTTPClient) SendMessage(ctx context.Context, token string, chatID int64, message SendMessage) (int64, error) {
	payload := struct {
		ChatID          int64            `json:"chat_id"`
		Text            string           `json:"text"`
		ParseMode       string           `json:"parse_mode,omitempty"`
		ReplyMarkup     *ReplyMarkup     `json:"reply_markup,omitempty"`
		ReplyParameters *ReplyParameters `json:"reply_parameters,omitempty"`
	}{chatID, message.Text, message.ParseMode, message.ReplyMarkup, message.ReplyParameters}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.call(ctx, token, "sendMessage", payload, &result); err != nil {
		return 0, err
	}
	if result.MessageID == 0 {
		return 0, &TelegramError{Class: "telegram_invalid_response", Retryable: false}
	}
	return result.MessageID, nil
}

func (c *TelegramHTTPClient) AnswerCallback(ctx context.Context, token string, callback AnswerCallback) error {
	payload := struct {
		CallbackQueryID string `json:"callback_query_id"`
		Text            string `json:"text,omitempty"`
		ShowAlert       bool   `json:"show_alert,omitempty"`
	}{callback.CallbackQueryID, callback.Text, callback.ShowAlert}
	var result bool
	if err := c.call(ctx, token, "answerCallbackQuery", payload, &result); err != nil {
		return err
	}
	if !result {
		return fmt.Errorf("%w", &TelegramError{Class: "telegram_invalid_response", Retryable: false})
	}
	return nil
}
