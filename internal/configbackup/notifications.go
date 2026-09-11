package configbackup

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/skrashevich/botmux/internal/models"
)

// Workload credentials are authentication verifiers, never plaintext tokens or
// ciphertext sealed by an instance key. Rotation is represented by Enabled.
type Workload struct {
	Ref                string             `json:"ref"`
	Name               string             `json:"name"`
	Status             string             `json:"status"`
	ServicePermissions ServicePermissions `json:"service_permissions"`
	RoutePermissions   []RoutePermission  `json:"route_permissions"`
	Credentials        []Credential       `json:"credentials"`
}
type ServicePermissions struct {
	Publish bool `json:"publish"`
	Query   bool `json:"query"`
}
type RoutePermission struct {
	RouteKey string `json:"route_key"`
	Action   string `json:"action"`
}
type Credential struct {
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	Verifier  string `json:"verifier"`
	Enabled   bool   `json:"enabled"`
}

// A service's identity is its source and full fingerprint, not a database ID.
type NotificationService struct {
	WorkloadRef string `json:"workload_ref"`
	Fingerprint string `json:"fingerprint"`
	DisplayName string `json:"display_name"`
}
type Subscription struct {
	Ref            string `json:"ref"`
	WorkloadRef    string `json:"workload_ref"`
	Fingerprint    string `json:"fingerprint"`
	DestinationRef string `json:"destination_ref"`
	Active         bool   `json:"active"`
}

func (s Snapshot) validateNotifications() error {
	workloads, names, hashes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, w := range s.Workloads {
		path := fmt.Sprintf("snapshot.workloads[%d]", i)
		if !validRef.MatchString(w.Ref) || workloads[w.Ref] {
			return &FieldError{ErrInvalid, path + ".ref", "invalid or duplicate"}
		}
		if strings.TrimSpace(w.Name) == "" || names[w.Name] {
			return &FieldError{ErrInvalid, path + ".name", "required and unique"}
		}
		if w.Status != "active" && w.Status != "disabled" {
			return &FieldError{ErrInvalid, path + ".status", "invalid"}
		}
		if len(w.RoutePermissions) > 0 {
			return &FieldError{ErrUnsupported, path + ".route_permissions", "must be empty"}
		}
		workloads[w.Ref], names[w.Name] = true, true
		for j, c := range w.Credentials {
			cp := fmt.Sprintf("%s.credentials[%d]", path, j)
			if c.Algorithm != "sha256" {
				return &FieldError{ErrInvalid, cp + ".algorithm", "must be sha256"}
			}
			decoded, err := hex.DecodeString(c.Verifier)
			if err != nil || len(decoded) != 32 || strings.ToLower(c.Verifier) != c.Verifier || hashes[c.Verifier] {
				return &FieldError{ErrInvalid, cp + ".verifier", "invalid or duplicate SHA-256 verifier"}
			}
			hashes[c.Verifier] = true
		}
	}
	services := map[[2]string]bool{}
	for i, n := range s.NotificationServices {
		path := fmt.Sprintf("snapshot.notification_services[%d]", i)
		key := [2]string{n.WorkloadRef, n.Fingerprint}
		if !workloads[n.WorkloadRef] {
			return &FieldError{ErrInvalid, path + ".workload_ref", "missing"}
		}
		if !models.ValidServiceLabel(n.Fingerprint, 512) || services[key] {
			return &FieldError{ErrInvalid, path + ".fingerprint", "invalid or duplicate"}
		}
		// Automatically registered names default to the full fingerprint (up to 512).
		if !models.ValidServiceLabel(n.DisplayName, 512) {
			return &FieldError{ErrInvalid, path + ".display_name", "invalid"}
		}
		services[key] = true
	}
	destinations := map[string]Destination{}
	bots := map[string]Bot{}
	for _, d := range s.Destinations {
		destinations[d.Ref] = d
	}
	for _, b := range s.Bots {
		bots[b.Ref] = b
	}
	refs := map[string]bool{}
	recipients := map[[4]string]bool{}
	for i, p := range s.Subscriptions {
		path := fmt.Sprintf("snapshot.subscriptions[%d]", i)
		if !validRef.MatchString(p.Ref) || refs[p.Ref] {
			return &FieldError{ErrInvalid, path + ".ref", "invalid or duplicate"}
		}
		refs[p.Ref] = true
		if !services[[2]string{p.WorkloadRef, p.Fingerprint}] {
			return &FieldError{ErrInvalid, path + ".fingerprint", "missing service"}
		}
		d, ok := destinations[p.DestinationRef]
		if !ok {
			return &FieldError{ErrInvalid, path + ".destination_ref", "missing"}
		}
		if !p.Active {
			continue
		}
		prefix, secret, ok := strings.Cut(bots[d.BotRef].Token, ":")
		botID, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || secret == "" || err != nil || botID <= 0 {
			return &FieldError{ErrInvalid, path + ".destination_ref", "invalid Telegram Bot identity"}
		}
		key := [4]string{p.WorkloadRef, p.Fingerprint, strconv.FormatInt(botID, 10), d.ChatID}
		if recipients[key] {
			return &FieldError{ErrInvalid, path + ".destination_ref", "duplicate active Bot/chat subscription"}
		}
		recipients[key] = true
	}
	return nil
}

func (s Snapshot) canonicalNotifications() Snapshot {
	s.Workloads = append([]Workload{}, s.Workloads...)
	for i := range s.Workloads {
		w := &s.Workloads[i]
		w.Credentials = append([]Credential{}, w.Credentials...)
		w.RoutePermissions = append([]RoutePermission{}, w.RoutePermissions...)
		sort.Slice(w.Credentials, func(i, j int) bool { return w.Credentials[i].Verifier < w.Credentials[j].Verifier })
	}
	s.NotificationServices = append([]NotificationService{}, s.NotificationServices...)
	s.Subscriptions = append([]Subscription{}, s.Subscriptions...)
	sort.Slice(s.Workloads, func(i, j int) bool { return s.Workloads[i].Ref < s.Workloads[j].Ref })
	sort.Slice(s.NotificationServices, func(i, j int) bool {
		a, b := s.NotificationServices[i], s.NotificationServices[j]
		if a.WorkloadRef != b.WorkloadRef {
			return a.WorkloadRef < b.WorkloadRef
		}
		return a.Fingerprint < b.Fingerprint
	})
	sort.Slice(s.Subscriptions, func(i, j int) bool { return s.Subscriptions[i].Ref < s.Subscriptions[j].Ref })
	return s
}
