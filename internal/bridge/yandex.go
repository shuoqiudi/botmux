package bridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rekurt/ymsdk/client"
	"github.com/rekurt/ymsdk/client/ym"
	"github.com/rekurt/ymsdk/client/ym/messages"
	"github.com/rekurt/ymsdk/client/ym/ymerrors"
	"github.com/skrashevich/botmux/internal/models"
)

// YandexConfig holds Yandex Messenger-specific bridge configuration (stored as JSON in BridgeConfig.Config).
type YandexConfig struct {
	BotToken      string `json:"bot_token"`
	WebhookSecret string `json:"webhook_secret"`
	APIBaseURL    string `json:"api_base_url,omitempty"`
}

func parseYandexConfig(configJSON string) (*YandexConfig, error) {
	if configJSON == "" {
		return nil, fmt.Errorf("empty yandex config")
	}
	var yc YandexConfig
	if err := json.Unmarshal([]byte(configJSON), &yc); err != nil {
		return nil, fmt.Errorf("invalid yandex config JSON: %w", err)
	}
	if yc.BotToken == "" {
		return nil, fmt.Errorf("yandex config missing bot_token")
	}
	return &yc, nil
}

func (bm *Manager) newYandexClient(cfg *YandexConfig) *client.YMClient {
	ymCfg := ym.Config{
		Token: cfg.BotToken,
		ErrorHandling: ymerrors.ErrorHandlingConfig{
			RetryStrategy: ymerrors.RetryStrategy{
				MaxAttempts:    2,
				InitialBackoff: 300 * time.Millisecond,
				MaxBackoff:     2 * time.Second,
			},
			RateLimitHandling: ymerrors.RateLimitHandling{
				UseRetryAfter:  true,
				DefaultBackoff: time.Second,
			},
		},
	}
	if cfg.APIBaseURL != "" {
		ymCfg.BaseURL = cfg.APIBaseURL
	}
	return client.Wrap(ym.NewClientWithHTTP(ymCfg, bm.client))
}

// VerifyYandexWebhookSecret validates the X-Webhook-Secret header.
func VerifyYandexWebhookSecret(secret string, header http.Header) bool {
	if secret == "" {
		return true
	}
	got := header.Get("X-Webhook-Secret")
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// HandleYandexEvent processes an incoming Yandex Messenger webhook update.
func (bm *Manager) HandleYandexEvent(bridgeID int64, header http.Header, body []byte) ([]byte, string, int, error) {
	bm.mu.RLock()
	cfg, ok := bm.bridges[bridgeID]
	bm.mu.RUnlock()
	if !ok {
		return nil, "", 404, fmt.Errorf("bridge %d not active", bridgeID)
	}

	yandexCfg, err := parseYandexConfig(cfg.Config)
	if err != nil {
		return nil, "", 500, fmt.Errorf("bridge %d yandex config error: %w", bridgeID, err)
	}

	if yandexCfg.WebhookSecret != "" && !VerifyYandexWebhookSecret(yandexCfg.WebhookSecret, header) {
		return nil, "", 401, fmt.Errorf("invalid webhook secret")
	}
	if yandexCfg.WebhookSecret == "" {
		log.Printf("[bridge] WARNING: Yandex bridge %d has no webhook_secret - requests are NOT verified", bridgeID)
	}

	var upd ym.Update
	if err := json.Unmarshal(body, &upd); err != nil {
		return nil, "", 400, fmt.Errorf("invalid yandex update JSON: %w", err)
	}

	incoming, ok := yandexUpdateToIncoming(upd)
	if !ok {
		return []byte(`{"ok":true}`), "application/json", 200, nil
	}

	if err := bm.HandleIncoming(bridgeID, incoming); err != nil {
		log.Printf("[bridge] id=%d yandex HandleIncoming error: %v", bridgeID, err)
		return nil, "", 400, err
	}

	return []byte(`{"ok":true}`), "application/json", 200, nil
}

func yandexUpdateToIncoming(upd ym.Update) (models.BridgeIncomingMessage, bool) {
	if upd.Chat == nil || upd.From == nil || upd.MessageID == 0 {
		return models.BridgeIncomingMessage{}, false
	}
	if upd.From.Robot != nil && *upd.From.Robot {
		return models.BridgeIncomingMessage{}, false
	}

	username := string(upd.From.Login)
	if upd.From.DisplayName != "" {
		username = upd.From.DisplayName
	} else if upd.From.Name != "" {
		username = upd.From.Name
	}

	extUserID := string(upd.From.Login)
	if extUserID == "" {
		extUserID = upd.From.ID
	}

	incoming := models.BridgeIncomingMessage{
		ExternalChatID: string(upd.Chat.ID),
		ExternalUserID: extUserID,
		Username:       username,
		Text:           upd.Text,
		ExternalMsgID:  strconv.FormatInt(int64(upd.MessageID), 10),
	}
	if upd.ThreadID != nil {
		incoming.ReplyToMsgID = strconv.FormatInt(int64(*upd.ThreadID), 10)
	}

	switch {
	case upd.BotRequest != nil && upd.BotRequest.ServerAction != nil:
		action := upd.BotRequest.ServerAction
		incoming.Text = fmt.Sprintf("[button:%s]", action.Name)
		if len(action.Payload) > 0 {
			incoming.Text += " " + string(action.Payload)
		}
	case upd.Image != nil && upd.Image.FileID != "":
		incoming.MediaType = "photo"
		incoming.FileID = upd.Image.FileID
		if incoming.FileName == "" {
			incoming.FileName = upd.Image.Name
		}
	case len(upd.Images) > 0 && upd.Images[0].FileID != "":
		incoming.MediaType = "photo"
		incoming.FileID = upd.Images[0].FileID
		incoming.FileName = upd.Images[0].Name
		if len(upd.Images) > 1 {
			extra := fmt.Sprintf("(+%d images)", len(upd.Images)-1)
			if incoming.Text != "" {
				incoming.Text += " " + extra
			} else {
				incoming.Text = extra
			}
		}
	case upd.Document != nil && upd.Document.ID != "":
		incoming.MediaType = "document"
		incoming.FileID = upd.Document.ID
		incoming.FileName = upd.Document.Name
	case upd.Sticker != nil:
		if upd.Sticker.ID != "" {
			incoming.MediaType = "sticker"
			incoming.FileID = upd.Sticker.ID
		}
		if incoming.Text == "" && upd.Sticker.Emoji != "" {
			incoming.Text = upd.Sticker.Emoji
		}
	case upd.Text != "":
		// plain text only
	default:
		return models.BridgeIncomingMessage{}, false
	}

	if incoming.Text == "" && incoming.MediaType == "" {
		return models.BridgeIncomingMessage{}, false
	}
	return incoming, true
}

func (bm *Manager) notifyYandexOutgoing(
	cfg *models.BridgeConfig,
	botID int64,
	extChatID, text, mediaType, fileID, fileName string,
	replyToMsgID int,
	yandexButtons any,
) {
	yandexCfg, err := parseYandexConfig(cfg.Config)
	if err != nil {
		log.Printf("[bridge] id=%d yandex config error for outgoing: %v", cfg.ID, err)
		bm.store.UpdateBridgeActivity(cfg.ID, fmt.Sprintf("yandex config error: %v", err))
		return
	}

	cs := bm.newYandexClient(yandexCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	replyOpts := yandexReplyOptions(cfg.ID, replyToMsgID, bm.store)
	suggestButtons, _ := yandexButtons.(*ym.SuggestButtons)
	chatID := ym.ChatID(extChatID)

	if strings.HasPrefix(fileID, bridgeFilePrefix) {
		fileID = ""
		mediaType = ""
	}

	if mediaType != "" && fileID != "" {
		if err := bm.sendYandexMedia(ctx, cs, chatID, text, mediaType, fileID, fileName, botID, suggestButtons); err != nil {
			log.Printf("[bridge] id=%d yandex media send failed: %v", cfg.ID, err)
			bm.store.UpdateBridgeActivity(cfg.ID, fmt.Sprintf("yandex error: %v", err))
			return
		}
		log.Printf("[bridge] id=%d yandex media sent to chat=%s type=%s", cfg.ID, extChatID, mediaType)
		bm.store.UpdateBridgeActivity(cfg.ID, "")
		return
	}

	opts := replyOpts
	if suggestButtons != nil {
		if opts == nil {
			opts = &messages.SendMessageOptions{}
		}
		opts.SuggestButtons = suggestButtons
	}

	if _, err := cs.Messages.SendToChat(ctx, chatID, text, opts); err != nil {
		log.Printf("[bridge] id=%d yandex sendText failed: %v", cfg.ID, err)
		bm.store.UpdateBridgeActivity(cfg.ID, fmt.Sprintf("yandex error: %v", err))
		return
	}

	log.Printf("[bridge] id=%d yandex message sent to chat=%s", cfg.ID, extChatID)
	bm.store.UpdateBridgeActivity(cfg.ID, "")
}

func yandexReplyOptions(bridgeID int64, replyToMsgID int, store interface {
	GetBridgeMsgMappingReverse(bridgeID int64, telegramMsgID int) (string, error)
}) *messages.SendMessageOptions {
	if replyToMsgID == 0 {
		return nil
	}
	extMsgID, err := store.GetBridgeMsgMappingReverse(bridgeID, replyToMsgID)
	if err != nil || extMsgID == "" {
		return nil
	}
	msgID, parseErr := strconv.ParseInt(extMsgID, 10, 64)
	if parseErr != nil {
		return nil
	}
	id := ym.MessageID(msgID)
	return &messages.SendMessageOptions{ReplyToMessageID: &id}
}

func (bm *Manager) sendYandexMedia(
	ctx context.Context,
	cs *client.YMClient,
	chatID ym.ChatID,
	caption, mediaType, fileID, fileName string,
	botID int64,
	suggestButtons *ym.SuggestButtons,
) error {
	data, name, err := bm.downloadTelegramFile(botID, fileID)
	if err != nil {
		return fmt.Errorf("download telegram file: %w", err)
	}
	if fileName != "" {
		name = fileName
	}
	if name == "" {
		name = "file"
	}

	switch mediaType {
	case "photo", "live_photo", "sticker", "animation":
		if caption != "" {
			_, err = cs.Messages.SendGallery(ctx, &messages.SendGalleryRequest{
				ChatID:         &chatID,
				Text:           caption,
				Images:         []messages.FilePart{{Reader: bytes.NewReader(data), Filename: name}},
				SuggestButtons: suggestButtons,
			})
			return err
		}
		_, err = cs.Messages.SendImage(ctx, &messages.SendImageRequest{
			ChatID:         &chatID,
			Image:          bytes.NewReader(data),
			Filename:       name,
			SuggestButtons: suggestButtons,
		})
		return err
	default:
		_, err = cs.Messages.SendFile(ctx, &messages.SendFileRequest{
			ChatID:         &chatID,
			Document:       bytes.NewReader(data),
			Filename:       name,
			SuggestButtons: suggestButtons,
		})
		return err
	}
}

// IsYandexBridge checks if a bridge uses the Yandex Messenger protocol.
func IsYandexBridge(cfg *models.BridgeConfig) bool {
	return strings.EqualFold(cfg.Protocol, "yandex")
}

// TelegramInlineKeyboardToYandex converts Telegram inline_keyboard JSON to Yandex SuggestButtons.
func TelegramInlineKeyboardToYandex(raw json.RawMessage) *ym.SuggestButtons {
	if len(raw) == 0 {
		return nil
	}
	var markup struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			URL          string `json:"url"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal(raw, &markup); err != nil || len(markup.InlineKeyboard) == 0 {
		return nil
	}

	var rows [][]ym.InlineSuggestButton
	for _, row := range markup.InlineKeyboard {
		var buttons []ym.InlineSuggestButton
		for _, btn := range row {
			if btn.Text == "" {
				continue
			}
			var directives []ym.Directive
			switch {
			case btn.URL != "":
				directives = []ym.Directive{{Type: ym.DirectiveOpenURI, URI: btn.URL}}
			case btn.CallbackData != "":
				directives = []ym.Directive{{Type: ym.DirectiveSendMessage, Text: btn.CallbackData}}
			default:
				directives = []ym.Directive{{Type: ym.DirectiveSendMessage, Text: btn.Text}}
			}
			buttons = append(buttons, ym.InlineSuggestButton{
				Title:      btn.Text,
				Directives: directives,
			})
		}
		if len(buttons) > 0 {
			rows = append(rows, buttons)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return &ym.SuggestButtons{Buttons: rows}
}
