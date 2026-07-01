package bot

import (
	"strings"
)

// ExtractRichText returns a readable plain-text fallback for Bot API 10.1
// rich_message payloads. The Telegram schema is intentionally represented as
// generic JSON here so botmux can preserve new rich-message updates even when
// the typed Telegram SDK is behind the upstream Bot API.
func ExtractRichText(value any) string {
	var parts []string
	collectRichText(value, &parts)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectRichText(value any, parts *[]string) {
	switch v := value.(type) {
	case map[string]any:
		if text, ok := v["text"].(string); ok && strings.TrimSpace(text) != "" {
			*parts = append(*parts, text)
		}
		for _, key := range []string{"caption", "title", "subtitle", "url"} {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				*parts = append(*parts, s)
			}
		}
		for _, key := range []string{"text", "texts", "blocks", "items", "rows", "cells", "caption", "header", "footer", "quote", "author"} {
			if child, ok := v[key]; ok {
				collectRichText(child, parts)
			}
		}
	case []any:
		for _, item := range v {
			collectRichText(item, parts)
		}
	}
}
