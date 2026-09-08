package gateway

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/skrashevich/botmux/internal/models"
)

const managementRequestSchema = "it_manage.management-request/v1"
const managementResultSchema = "it_manage.management-result/v1"

type managementIdentity struct {
	RequestID  string `json:"request_id"`
	DeliveryID string `json:"delivery_id"`
}

type managementRequest struct {
	Schema   string             `json:"schema"`
	Argv     []string           `json:"argv"`
	Identity managementIdentity `json:"identity"`
	Source   map[string]string  `json:"source"`
}

type adapterMessage struct {
	Text      string `json:"text"`
	MessageID int64  `json:"message_id"`
	ThreadID  int64  `json:"message_thread_id"`
}

// Only original messages enter the command boundary. Edits, callbacks and
// unrelated Updates remain durably accepted but never execute business work.
func parseManagementRequest(d *models.InboundDelivery, username string) (*managementRequest, adapterMessage, error) {
	var update struct {
		Message     *adapterMessage `json:"message"`
		ChannelPost *adapterMessage `json:"channel_post"`
	}
	if json.Unmarshal(d.RawUpdate, &update) != nil {
		return nil, adapterMessage{}, errors.New("invalid_request")
	}
	msg := update.Message
	if msg == nil {
		msg = update.ChannelPost
	}
	if msg == nil {
		return nil, adapterMessage{}, nil
	}
	command, rest, _ := strings.Cut(msg.Text, " ")
	// Tabs and newlines also delimit the command, but controls in argv fail
	// contract validation below; they never become shell syntax.
	if end := strings.IndexFunc(msg.Text, unicode.IsSpace); end >= 0 {
		command, rest = msg.Text[:end], msg.Text[end:]
	}
	expected := "/it_manage"
	if command != expected && !strings.EqualFold(command, expected+"@"+username) {
		return nil, *msg, nil
	}
	argv, err := splitManagementArguments(rest)
	if err != nil {
		return nil, *msg, err
	}
	return &managementRequest{Schema: managementRequestSchema, Argv: argv,
		Identity: managementIdentity{RequestID: d.DeliveryID, DeliveryID: d.DeliveryID},
		Source:   map[string]string{"type": "telegram_gateway", "route_key": d.RouteKey, "update_id": strconv.FormatInt(d.UpdateID, 10)}}, *msg, nil
}

// Generic quote/escape tokenization only: no expansion, execution, options,
// modules, actions or business rules are interpreted here.
func splitManagementArguments(text string) ([]string, error) {
	invalid := errors.New("invalid_request")
	if len(text) > 64*1024+1024 || !utf8.ValidString(text) {
		return nil, invalid
	}
	var args []string
	var token strings.Builder
	var quote rune
	escaped, started := false, false
	flush := func() bool {
		if !started {
			return true
		}
		arg := token.String()
		if strings.TrimSpace(arg) == "" || len(arg) > 4096 {
			return false
		}
		args = append(args, arg)
		token.Reset()
		started = false
		return len(args) <= 128
	}
	total := 0
	for _, ch := range text {
		if ch < 32 || ch == 127 {
			return nil, invalid
		}
		if escaped {
			token.WriteRune(ch)
			escaped = false
			started = true
			continue
		}
		if ch == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				token.WriteRune(ch)
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			started = true
			continue
		}
		if unicode.IsSpace(ch) {
			if !flush() {
				return nil, invalid
			}
			continue
		}
		token.WriteRune(ch)
		started = true
	}
	if escaped || quote != 0 || !flush() || len(args) == 0 {
		return nil, invalid
	}
	for _, arg := range args {
		total += len(arg)
	}
	if total > 64*1024 {
		return nil, invalid
	}
	return args, nil
}
