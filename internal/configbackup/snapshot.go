// Package configbackup defines the portable configuration boundary, without
// database IDs, ciphertext, runtime state or instance-specific fingerprints.
package configbackup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var ErrInvalid = errors.New("invalid configuration snapshot")
var ErrUnsupported = errors.New("unsupported configuration")
var ErrConflict = errors.New("target configuration conflicts with snapshot")

// FieldError carries only a schema path and a server-defined explanation.
// Neither includes the rejected value or an unknown caller-supplied key.
type FieldError struct {
	Kind   error
	Path   string
	Reason string
}

func (e *FieldError) Error() string { return fmt.Sprintf("%s: %s %s", e.Kind, e.Path, e.Reason) }
func (e *FieldError) Unwrap() error { return e.Kind }

// All supported collections share the document and Store transaction boundary.
type Snapshot struct {
	SchemaVersion        int                   `json:"schema_version"`
	Bots                 []Bot                 `json:"bots"`
	Destinations         []Destination         `json:"destinations"`
	ConditionalRoutes    []ConditionalRoute    `json:"conditional_routes"`
	NotificationServices []NotificationService `json:"notification_services"`
	Subscriptions        []Subscription        `json:"subscriptions"`
	BusinessRoutes       []BusinessRoute       `json:"business_routes"`
	Workloads            []Workload            `json:"workloads"`
}
type Bot struct {
	Ref             string `json:"ref"`
	Name            string `json:"name"`
	Token           string `json:"token"`
	Username        string `json:"username"`
	Description     string `json:"description"`
	ManageEnabled   bool   `json:"manage_enabled"`
	ProxyEnabled    bool   `json:"proxy_enabled"`
	LongPollEnabled bool   `json:"long_poll_enabled"`
	Disabled        bool   `json:"disabled"`
	BackendURL      string `json:"backend_url"`
	SecretToken     string `json:"secret_token"`
	PollingTimeout  int    `json:"polling_timeout"`
	Source          string `json:"source"`
}
type Destination struct {
	Ref    string `json:"ref"`
	BotRef string `json:"bot_ref"`
	Name   string `json:"name"`
	ChatID string `json:"chat_id"`
	Status string `json:"status"`
}

// ConditionalRoutes execute in array order. Source chat "0" means any chat;
// target chat "0" means the incoming chat.
type ConditionalRoute struct {
	Ref            string `json:"ref"`
	SourceBotRef   string `json:"source_bot_ref"`
	TargetBotRef   string `json:"target_bot_ref"`
	SourceChatID   string `json:"source_chat_id"`
	TargetChatID   string `json:"target_chat_id"`
	ConditionType  string `json:"condition_type"`
	ConditionValue string `json:"condition_value"`
	Action         string `json:"action"`
	Description    string `json:"description"`
	Enabled        bool   `json:"enabled"`
}
type Receipt struct {
	BusinessRoutes          int      `json:"business_routes"`
	Workloads               int      `json:"workloads"`
	NotificationServices    int      `json:"notification_services"`
	Subscriptions           int      `json:"subscriptions"`
	ConditionalRoutes       int      `json:"conditional_routes"`
	Digest                  string   `json:"digest"`
	Bots                    int      `json:"bots"`
	Destinations            int      `json:"destinations"`
	ConfigurationCommitted  bool     `json:"configuration_committed"`
	Replayed                bool     `json:"replayed"`
	RuntimeLoaded           bool     `json:"runtime_loaded"`
	RuntimeFailedRefs       []string `json:"runtime_failed_refs"`
	RuntimeFailedComponents []string `json:"runtime_failed_components"`
	ExternalHealth          string   `json:"external_health"`
}

func Empty() Snapshot {
	return Snapshot{1, []Bot{}, []Destination{}, []ConditionalRoute{}, []NotificationService{}, []Subscription{}, []BusinessRoute{}, []Workload{}}
}

// Decode rejects unknown, missing, null and duplicate fields, including nested
// objects. Errors never echo supplied keys or secret-bearing values.
func Decode(raw []byte) (Snapshot, error) {
	var s Snapshot
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := checkShape(dec, reflect.TypeOf(s), "snapshot"); err != nil {
		return s, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return s, fmt.Errorf("%w: trailing content", ErrInvalid)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%w: incorrect field type", ErrInvalid)
	}
	return s, s.Validate()
}
func checkShape(d *json.Decoder, t reflect.Type, path string) error {
	tok, err := d.Token()
	if err != nil || tok == nil {
		return &FieldError{ErrInvalid, path, "missing or invalid"}
	}
	switch t.Kind() {
	case reflect.Struct:
		if tok != json.Delim('{') {
			return &FieldError{ErrInvalid, path, "must be an object"}
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fields[f.Tag.Get("json")] = f.Type
		}
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return ErrInvalid
			}
			k, ok := key.(string)
			ft, known := fields[k]
			if !ok || !known || seen[k] {
				return &FieldError{ErrInvalid, path, "unknown or duplicate field"}
			}
			seen[k] = true
			if err := checkShape(d, ft, path+"."+k); err != nil {
				return err
			}
		}
		if _, err := d.Token(); err != nil {
			return ErrInvalid
		}
		for k := range fields {
			if !seen[k] {
				return &FieldError{ErrInvalid, path + "." + k, "required"}
			}
		}
	case reflect.Slice:
		if tok != json.Delim('[') {
			return &FieldError{ErrInvalid, path, "must be an array"}
		}
		if t.Elem() == reflect.TypeOf(json.RawMessage{}) {
			if d.More() {
				return &FieldError{ErrUnsupported, path, "must be empty"}
			}
		} else {
			for i := 0; d.More(); i++ {
				if err := checkShape(d, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		if _, err := d.Token(); err != nil {
			return ErrInvalid
		}
	case reflect.String:
		if _, ok := tok.(string); !ok {
			return &FieldError{ErrInvalid, path, "must be a string"}
		}
	case reflect.Bool:
		if _, ok := tok.(bool); !ok {
			return &FieldError{ErrInvalid, path, "must be a boolean"}
		}
	case reflect.Int:
		if _, ok := tok.(json.Number); !ok {
			return &FieldError{ErrInvalid, path, "must be an integer"}
		}
	}
	return nil
}

var validRef = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (s Snapshot) Validate() error {
	if s.SchemaVersion != 1 {
		return &FieldError{ErrInvalid, "snapshot.schema_version", "must be 1"}
	}
	refs := map[string]bool{}
	tokens := map[string]bool{}
	for i, b := range s.Bots {
		if !validRef.MatchString(b.Ref) || refs[b.Ref] {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.bots[%d].ref", i), "invalid or duplicate"}
		}
		if strings.TrimSpace(b.Token) == "" || tokens[b.Token] {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.bots[%d].token", i), "required and unique"}
		}
		if b.PollingTimeout < 0 {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.bots[%d].polling_timeout", i), "negative"}
		}
		refs[b.Ref] = true
		tokens[b.Token] = true
	}
	targets := map[string]bool{}
	for i, d := range s.Destinations {
		if !validRef.MatchString(d.Ref) || targets[d.Ref] {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.destinations[%d].ref", i), "invalid or duplicate"}
		}
		if !refs[d.BotRef] {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.destinations[%d].bot_ref", i), "missing"}
		}
		id, err := strconv.ParseInt(d.ChatID, 10, 64)
		if err != nil || id == 0 || strconv.FormatInt(id, 10) != d.ChatID {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.destinations[%d].chat_id", i), "invalid"}
		}
		if d.Status != "active" && d.Status != "disabled" {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.destinations[%d].status", i), "invalid"}
		}
		targets[d.Ref] = true
	}
	routeRefs := map[string]bool{}
	for i, r := range s.ConditionalRoutes {
		fieldError := func(field, reason string) error {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.conditional_routes[%d].%s", i, field), reason}
		}
		if !validRef.MatchString(r.Ref) || routeRefs[r.Ref] {
			return fieldError("ref", "invalid or duplicate")
		}
		routeRefs[r.Ref] = true
		if !refs[r.SourceBotRef] {
			return fieldError("source_bot_ref", "missing")
		}
		if !refs[r.TargetBotRef] {
			return fieldError("target_bot_ref", "missing")
		}
		for _, chat := range []struct{ field, value string }{{"source_chat_id", r.SourceChatID}, {"target_chat_id", r.TargetChatID}} {
			id, err := strconv.ParseInt(chat.value, 10, 64)
			if err != nil || strconv.FormatInt(id, 10) != chat.value {
				return fieldError(chat.field, "invalid")
			}
		}
		switch r.ConditionType {
		case "text":
			if _, err := regexp.Compile("(?i)" + r.ConditionValue); err != nil {
				return fieldError("condition_value", "invalid regular expression")
			}
		case "user_id", "chat_id":
			if _, err := strconv.ParseInt(r.ConditionValue, 10, 64); err != nil {
				return fieldError("condition_value", "invalid integer")
			}
		default:
			return fieldError("condition_type", "unsupported")
		}
		switch r.Action {
		case "forward", "copy", "drop":
		default:
			return fieldError("action", "unsupported")
		}
	}
	if err := s.validateBusinessRoutes(); err != nil {
		return err
	}
	return s.validateNotifications()
}
func (s Snapshot) Canonical() ([]byte, string, error) {
	s = s.canonicalNotifications()
	s = s.canonicalBusinessRoutes()
	// Route order is semantic: never sort by random configuration identity.
	s.ConditionalRoutes = append([]ConditionalRoute{}, s.ConditionalRoutes...)
	s.Bots = append([]Bot{}, s.Bots...)
	s.Destinations = append([]Destination{}, s.Destinations...)
	sort.Slice(s.Bots, func(i, j int) bool { return s.Bots[i].Ref < s.Bots[j].Ref })
	sort.Slice(s.Destinations, func(i, j int) bool { return s.Destinations[i].Ref < s.Destinations[j].Ref })
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, "", err
	}
	raw = append(raw, '\n')
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}
