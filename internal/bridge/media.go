package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

const bridgeFilePrefix = "bridge:"

// BridgeFileID encodes an external protocol file reference for storage in messages.file_id.
func BridgeFileID(bridgeID int64, externalFileID string) string {
	return fmt.Sprintf("%s%d:%s", bridgeFilePrefix, bridgeID, externalFileID)
}

// ParseBridgeFileID splits a bridge-encoded file_id. Returns ok=false for normal Telegram file IDs.
func ParseBridgeFileID(fileID string) (bridgeID int64, externalFileID string, ok bool) {
	if !strings.HasPrefix(fileID, bridgeFilePrefix) {
		return 0, "", false
	}
	rest := strings.TrimPrefix(fileID, bridgeFilePrefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, "", false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return id, parts[1], true
}

func applyMediaToSyntheticMessage(msgMap map[string]any, msg models.BridgeIncomingMessage, bridgeID int64) {
	if msg.MediaType == "" || msg.FileID == "" {
		return
	}
	fileID := BridgeFileID(bridgeID, msg.FileID)
	caption := msg.Text
	delete(msgMap, "text")

	switch msg.MediaType {
	case "photo", "live_photo", "image":
		entry := map[string]any{"file_id": fileID, "width": 100, "height": 100}
		msgMap["photo"] = []map[string]any{entry}
		if caption != "" {
			msgMap["caption"] = caption
		}
	case "sticker":
		msgMap["sticker"] = map[string]any{"file_id": fileID, "is_animated": false}
	case "video", "animation", "video_note":
		msgMap[msg.MediaType] = map[string]any{"file_id": fileID}
		if caption != "" {
			msgMap["caption"] = caption
		}
	case "voice", "audio":
		msgMap[msg.MediaType] = map[string]any{"file_id": fileID, "mime_type": "audio/mpeg"}
		if caption != "" {
			msgMap["caption"] = caption
		}
	default: // document and other file-like types
		name := msg.FileName
		if name == "" {
			name = "file"
		}
		msgMap["document"] = map[string]any{"file_id": fileID, "file_name": name}
		if caption != "" {
			msgMap["caption"] = caption
		}
	}
}

func (bm *Manager) downloadTelegramFile(botID int64, fileID string) ([]byte, string, error) {
	cfg, err := bm.store.GetBotConfig(botID)
	if err != nil || cfg.Token == "" {
		return nil, "", fmt.Errorf("bot %d not found or has no token", botID)
	}

	base := bm.tgAPIBaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}

	getFileURL := fmt.Sprintf("%s/bot%s/getFile?file_id=%s", base, cfg.Token, url.QueryEscape(fileID))
	resp, err := bm.client.Get(getFileURL)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	var fileResp struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&fileResp); err != nil || !fileResp.OK || fileResp.Result.FilePath == "" {
		return nil, "", fmt.Errorf("getFile failed")
	}

	downloadURL := fmt.Sprintf("%s/file/bot%s/%s", base, cfg.Token, fileResp.Result.FilePath)
	dlResp, err := bm.client.Get(downloadURL)
	if err != nil {
		return nil, "", err
	}
	defer dlResp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(dlResp.Body, 50<<20))
	if err != nil {
		return nil, "", err
	}
	return data, path.Base(fileResp.Result.FilePath), nil
}

// ServeBridgeMedia streams a file from an external bridge protocol (used by /api/media).
func (bm *Manager) ServeBridgeMedia(w http.ResponseWriter, bridgeID int64, externalFileID string) error {
	bm.mu.RLock()
	cfg := bm.bridges[bridgeID]
	bm.mu.RUnlock()
	if cfg == nil {
		return fmt.Errorf("bridge %d not active", bridgeID)
	}
	if !IsYandexBridge(cfg) {
		return fmt.Errorf("bridge %d does not support media proxy", bridgeID)
	}

	yandexCfg, err := parseYandexConfig(cfg.Config)
	if err != nil {
		return err
	}

	cs := bm.newYandexClient(yandexCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body, meta, err := cs.Messages.GetFile(ctx, externalFileID)
	if err != nil {
		return err
	}
	defer body.Close()

	if meta != nil && meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	if meta != nil && meta.ContentLength > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.ContentLength, 10))
	}
	_, err = io.Copy(w, body)
	return err
}
