package gateway

import (
	"encoding/json"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/skrashevich/botmux/internal/models"
)

// The producer bounds serialized command_output (including escaping) to 1 MiB.
// Keep the prepared payload private: operational API state must not expose text.
const commandOutputLimit = 1 << 20

type managementResult struct {
	Schema     string          `json:"schema"`
	RequestID  string          `json:"request_id"`
	DeliveryID string          `json:"delivery_id"`
	Status     string          `json:"status"`
	Code       string          `json:"code"`
	Output     json.RawMessage `json:"command_output"`
}

type commandOutput struct {
	Format   string  `json:"format"`
	Encoding string  `json:"encoding"`
	Text     *string `json:"text"`
	State    string  `json:"state"`
	Reason   string  `json:"reason"`
}

func resultReply(state *models.AdapterExecution, message adapterMessage, result *managementResult) outboundEnvelope {
	status := "Request " + state.ResultStatus + "."
	if state.ResultCode != "" {
		status += " Code: " + state.ResultCode + "."
	}
	status += " Request: " + state.DeliveryID
	send := &SendMessage{Text: status, MessageThreadID: message.ThreadID}
	if message.MessageID > 0 {
		send.ReplyParameters = &ReplyParameters{MessageID: message.MessageID, AllowSendingWithoutReply: true}
	}
	envelope := outboundEnvelope{Kind: "message", Message: send}
	if result == nil {
		send.Text += "\nCommand Output unavailable."
		return envelope
	}
	raw := result.Output
	if len(raw) == 0 {
		send.Text += "\nCommand Output unknown (legacy result)."
		return envelope
	}
	var output commandOutput
	if len(raw) > commandOutputLimit {
		send.Text += "\nCommand Output unavailable (output_limit_exceeded)."
		return envelope
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &output) != nil || output.Format != "text/plain" || output.Encoding != "utf-8" || output.Text == nil || !validOutput(output) {
		send.Text += "\nCommand Output unavailable (invalid_output_contract)."
		return envelope
	}
	if output.State == "unavailable" {
		send.Text += "\nCommand Output unavailable (" + output.Reason + ")."
		return envelope
	}
	label := "Command Output"
	if output.State == "partial" {
		label += " (partial: " + output.Reason + ")"
	}
	text := cleanTerminalOutput(*output.Text)
	if text == "" {
		send.Text += "\n" + label + ": empty."
		return envelope
	}
	plain := status + "\n" + label + ":\n" + text
	lines := strings.Count(strings.TrimSuffix(text, "\n"), "\n") + 1
	if lines <= 40 && utf8.RuneCountInString(text) <= 3500 && telegramTextLength(plain) <= 4096 {
		send.Text = html.EscapeString(status+"\n"+label+":\n") + "<pre>" + html.EscapeString(text) + "</pre>"
		send.ParseMode = "HTML"
		return envelope
	}
	return outboundEnvelope{Kind: "document", Document: &SendDocument{Filename: "command-output-" + state.DeliveryID + ".txt", Content: text,
		Caption: status + "\n" + label + " attached.", MessageThreadID: send.MessageThreadID, ReplyParameters: send.ReplyParameters}}
}

func validOutput(o commandOutput) bool {
	switch o.State {
	case "complete":
		return o.Reason == ""
	case "partial", "unavailable":
		if o.State == "unavailable" && *o.Text != "" {
			return false
		}
		switch o.Reason {
		case "execution_timeout", "execution_interrupted", "render_failed", "renderer_unavailable", "result_unavailable", "output_limit_exceeded":
			return true
		}
	}
	return false
}

// Telegram entities use UTF-16 offsets. Counting these units is conservative
// for the final rendered message, including correlation and status text.
func telegramTextLength(text string) int {
	n := 0
	for _, r := range text {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// Remove CSI, OSC and terminal control strings (including C1 forms). Keep
// ordinary spacing and Unicode; this is sanitation, never business rendering.
func cleanTerminalOutput(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out strings.Builder
	state := byte(0)
	for _, r := range text {
		switch state {
		case 's': // OSC/DCS/SOS/PM/APC, terminated by BEL or ST
			if r == 7 || r == 0x9c {
				state = 0
			} else if r == 27 {
				state = 't'
			}
			continue
		case 't':
			if r == '\\' || r == 0x9c || r == 7 {
				state = 0
			} else {
				state = 's'
			}
			continue
		case 'c':
			if r >= 0x40 && r <= 0x7e {
				state = 0
			}
			continue
		case 'e':
			switch r {
			case '[':
				state = 'c'
			case ']', 'P', 'X', '^', '_':
				state = 's'
			default:
				if r < 0x20 || r > 0x2f {
					state = 0
				}
			}
			continue
		}
		switch r {
		case 27:
			state = 'e'
		case 0x9b:
			state = 'c'
		case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
			state = 's'
		default:
			if r == '\n' || r == '\t' || (r >= 32 && !(r >= 0x7f && r <= 0x9f)) {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}
