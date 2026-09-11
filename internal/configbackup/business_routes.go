package configbackup

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// RouteKey is the portable identity. Bot and destination references deliberately
// avoid compatibility account IDs, which are local to each instance.
type BusinessRoute struct {
	RouteKey                string   `json:"route_key"`
	DisplayName             string   `json:"display_name"`
	BotRef                  string   `json:"bot_ref"`
	DestinationRef          string   `json:"destination_ref"`
	InboundTarget           string   `json:"inbound_target"`
	InboundEnabled          bool     `json:"inbound_enabled"`
	InboundBackendURL       string   `json:"inbound_backend_url"`
	InboundBackendHealthURL string   `json:"inbound_backend_health_url"`
	InboundBackendToken     string   `json:"inbound_backend_token"`
	OutboundEnabled         bool     `json:"outbound_enabled"`
	AllowedCallers          []string `json:"allowed_callers"`
	Enabled                 bool     `json:"enabled"`
	Status                  string   `json:"status"`
}

var routeKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (s Snapshot) validateBusinessRoutes() error {
	bots := map[string]Bot{}
	destinations := map[string]Destination{}
	for _, b := range s.Bots {
		bots[b.Ref] = b
	}
	for _, d := range s.Destinations {
		destinations[d.Ref] = d
	}
	keys := map[string]bool{}
	inbound := map[[2]string]bool{}
	for i, r := range s.BusinessRoutes {
		invalid := func(field, reason string) error {
			return &FieldError{ErrInvalid, fmt.Sprintf("snapshot.business_routes[%d].%s", i, field), reason}
		}
		if !routeKeyPattern.MatchString(r.RouteKey) || keys[r.RouteKey] {
			return invalid("route_key", "invalid or duplicate")
		}
		keys[r.RouteKey] = true
		if strings.TrimSpace(r.DisplayName) == "" {
			return invalid("display_name", "required")
		}
		b, ok := bots[r.BotRef]
		if !ok {
			return invalid("bot_ref", "missing")
		}
		d, ok := destinations[r.DestinationRef]
		if !ok || d.BotRef != r.BotRef {
			return invalid("destination_ref", "missing or belongs to another Bot")
		}
		if r.Status != "active" && r.Status != "disabled" {
			return invalid("status", "invalid")
		}
		switch r.InboundTarget {
		case "backend":
			if r.InboundEnabled {
				for _, endpoint := range []struct{ field, value string }{{"inbound_backend_url", r.InboundBackendURL}, {"inbound_backend_health_url", r.InboundBackendHealthURL}} {
					if endpoint.field == "inbound_backend_health_url" && endpoint.value == "" {
						continue
					}
					u, err := url.ParseRequestURI(endpoint.value)
					if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
						return invalid(endpoint.field, "invalid HTTP endpoint")
					}
				}
			}
		case "it_manage":
			if r.InboundBackendURL != "" || r.InboundBackendHealthURL != "" || r.InboundBackendToken != "" {
				return invalid("inbound_target", "embedded adapter cannot use backend configuration")
			}
			if r.InboundEnabled && !r.OutboundEnabled {
				return invalid("outbound_enabled", "required by embedded adapter")
			}
		default:
			return invalid("inbound_target", "unsupported")
		}
		if r.Enabled && r.InboundEnabled {
			prefix, secret, ok := strings.Cut(b.Token, ":")
			id, err := strconv.ParseInt(prefix, 10, 64)
			if !ok || secret == "" || err != nil || id <= 0 {
				return invalid("bot_ref", "invalid Telegram Bot identity")
			}
			key := [2]string{strconv.FormatInt(id, 10), d.ChatID}
			if inbound[key] {
				return invalid("destination_ref", "duplicate active inbound Bot/chat target")
			}
			inbound[key] = true
		}
	}
	return nil
}

func (s Snapshot) canonicalBusinessRoutes() Snapshot {
	s.BusinessRoutes = append([]BusinessRoute{}, s.BusinessRoutes...)
	for i := range s.BusinessRoutes {
		r := &s.BusinessRoutes[i]
		r.AllowedCallers = append([]string{}, r.AllowedCallers...)
		sort.Strings(r.AllowedCallers)
	}
	sort.Slice(s.BusinessRoutes, func(i, j int) bool { return s.BusinessRoutes[i].RouteKey < s.BusinessRoutes[j].RouteKey })
	return s
}
