package bot

import "testing"

func TestExtractRichText(t *testing.T) {
	got := ExtractRichText(map[string]any{
		"blocks": []any{
			map[string]any{
				"type": "paragraph",
				"text": map[string]any{
					"texts": []any{
						map[string]any{"type": "bold", "text": "Hello"},
						map[string]any{"type": "plain", "text": "world"},
					},
				},
			},
			map[string]any{
				"type":    "footer",
				"caption": "Generated",
			},
		},
	})
	want := "Hello\nworld\nGenerated"
	if got != want {
		t.Fatalf("ExtractRichText() = %q, want %q", got, want)
	}
}
