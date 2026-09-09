package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type TelegramFailureOutcome string

const (
	TelegramDefiniteTransient TelegramFailureOutcome = "definite_transient"
	TelegramAmbiguous         TelegramFailureOutcome = "ambiguous"
	TelegramPermanent         TelegramFailureOutcome = "permanent"
)

type TelegramError struct {
	Class         string
	Outcome       TelegramFailureOutcome
	RetryAfter    time.Duration
	WriteObserved bool
}

func (e *TelegramError) Error() string { return e.Class }

func (e *TelegramError) Retryable() bool { return e.Outcome == TelegramDefiniteTransient }

type TelegramHTTPClient struct {
	baseURL string
	client  *http.Client
}

func NewTelegramHTTPClient(baseURL string) *TelegramHTTPClient {
	return NewTelegramHTTPClientWithClient(baseURL, nil)
}

func NewTelegramHTTPClientWithClient(baseURL string, client *http.Client) *TelegramHTTPClient {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &TelegramHTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
	}
}

type telegramResponse struct {
	OK         bool            `json:"ok"`
	ErrorCode  int             `json:"error_code"`
	Result     json.RawMessage `json:"result"`
	Parameters struct {
		RetryAfter int64 `json:"retry_after"`
	} `json:"parameters"`
}

func (c *TelegramHTTPClient) call(ctx context.Context, token, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.callBody(ctx, token, method, body, "application/json", result)
}

func (c *TelegramHTTPClient) callBody(ctx context.Context, token, method string, body []byte, contentType string, result any) error {
	endpoint := c.baseURL + "/bot" + url.PathEscape(token) + "/" + method
	var wroteRequest atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &TelegramError{Class: "telegram_request_invalid", Outcome: TelegramPermanent}
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.client.Do(req)
	if err != nil {
		outcome := TelegramDefiniteTransient
		class := "telegram_unavailable"
		if wroteRequest.Load() {
			outcome = TelegramAmbiguous
			class = "telegram_send_ambiguous"
		}
		return &TelegramError{Class: class, Outcome: outcome, WriteObserved: wroteRequest.Load()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &TelegramError{Class: "telegram_send_ambiguous", Outcome: TelegramAmbiguous, WriteObserved: true}
	}
	var envelope telegramResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		if resp.StatusCode >= 500 {
			return &TelegramError{Class: "telegram_unavailable", Outcome: TelegramDefiniteTransient, WriteObserved: true}
		}
		return &TelegramError{Class: "telegram_response_ambiguous", Outcome: TelegramAmbiguous, WriteObserved: true}
	}
	if resp.StatusCode == http.StatusTooManyRequests || envelope.ErrorCode == http.StatusTooManyRequests {
		retryAfter := time.Duration(envelope.Parameters.RetryAfter) * time.Second
		return &TelegramError{Class: "telegram_rate_limited", Outcome: TelegramDefiniteTransient, RetryAfter: retryAfter, WriteObserved: true}
	}
	if resp.StatusCode >= 500 {
		return &TelegramError{Class: "telegram_unavailable", Outcome: TelegramDefiniteTransient, WriteObserved: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.OK {
		return &TelegramError{Class: "telegram_rejected", Outcome: TelegramPermanent, WriteObserved: true}
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return &TelegramError{Class: "telegram_response_ambiguous", Outcome: TelegramAmbiguous, WriteObserved: true}
		}
	}
	return nil
}

func (c *TelegramHTTPClient) SendMessage(ctx context.Context, token string, chatID int64, message SendMessage) (int64, error) {
	payload := struct {
		ChatID          int64            `json:"chat_id"`
		MessageThreadID int64            `json:"message_thread_id,omitempty"`
		Text            string           `json:"text"`
		ParseMode       string           `json:"parse_mode,omitempty"`
		ReplyMarkup     *ReplyMarkup     `json:"reply_markup,omitempty"`
		ReplyParameters *ReplyParameters `json:"reply_parameters,omitempty"`
	}{chatID, message.MessageThreadID, message.Text, message.ParseMode, message.ReplyMarkup, message.ReplyParameters}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.call(ctx, token, "sendMessage", payload, &result); err != nil {
		return 0, err
	}
	if result.MessageID == 0 {
		return 0, &TelegramError{Class: "telegram_response_ambiguous", Outcome: TelegramAmbiguous, WriteObserved: true}
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
		return fmt.Errorf("%w", &TelegramError{Class: "telegram_rejected", Outcome: TelegramPermanent, WriteObserved: true})
	}
	return nil
}

func (c *TelegramHTTPClient) ProbeBot(ctx context.Context, token string) error {
	var result struct {
		ID int64 `json:"id"`
	}
	if err := c.call(ctx, token, "getMe", struct{}{}, &result); err != nil {
		return err
	}
	if result.ID == 0 {
		return &TelegramError{Class: "telegram_rejected", Outcome: TelegramPermanent, WriteObserved: true}
	}
	return nil
}

func (c *TelegramHTTPClient) ProbeDestination(ctx context.Context, token string, chatID int64) error {
	var result struct {
		ID int64 `json:"id"`
	}
	if err := c.call(ctx, token, "getChat", struct {
		ChatID int64 `json:"chat_id"`
	}{ChatID: chatID}, &result); err != nil {
		return err
	}
	if result.ID == 0 {
		return &TelegramError{Class: "telegram_rejected", Outcome: TelegramPermanent, WriteObserved: true}
	}
	return nil
}

// Documents use the same request tracing and failure classification as messages.
// Content is durable text, never a URL, Telegram file ID or local temporary path.
func (c *TelegramHTTPClient) SendDocument(ctx context.Context, token string, chatID int64, document SendDocument) (int64, error) {
	if len(document.Content) == 0 || len(document.Content) > commandOutputLimit || !utf8.ValidString(document.Content) || telegramTextLength(document.Caption) > 1024 || !strings.HasSuffix(document.Filename, ".txt") || strings.ContainsAny(document.Filename, "\\/\r\n\"") {
		return 0, ErrInvalidPayload
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10), "caption": document.Caption}
	if document.MessageThreadID != 0 {
		fields["message_thread_id"] = strconv.FormatInt(document.MessageThreadID, 10)
	}
	if document.ReplyParameters != nil {
		raw, err := json.Marshal(document.ReplyParameters)
		if err != nil {
			return 0, err
		}
		fields["reply_parameters"] = string(raw)
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return 0, err
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="document"; filename="`+document.Filename+`"`)
	header.Set("Content-Type", "text/plain; charset=utf-8")
	part, err := writer.CreatePart(header)
	if err != nil {
		return 0, err
	}
	if _, err = io.WriteString(part, document.Content); err != nil {
		return 0, err
	}
	if err = writer.Close(); err != nil {
		return 0, err
	}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err = c.callBody(ctx, token, "sendDocument", body.Bytes(), writer.FormDataContentType(), &result); err != nil {
		return 0, err
	}
	if result.MessageID == 0 {
		return 0, &TelegramError{Class: "telegram_response_ambiguous", Outcome: TelegramAmbiguous, WriteObserved: true}
	}
	return result.MessageID, nil
}
