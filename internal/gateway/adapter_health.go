package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

// JenkinsHealth uses a credentialed, read-only job probe. It never returns an
// upstream URL, response body or authentication detail to the operator API.
func (a *Adapter) JenkinsHealth(ctx context.Context) models.GatewayComponentHealth {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if time.Since(a.probedAt) < 5*time.Second {
		return a.probeResult
	}
	now := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, _, raw, err := a.jenkins.call(ctx, http.MethodGet, a.jenkins.jobURL+"/api/json?tree=buildable", nil, 64*1024)
	result := models.GatewayComponentHealth{Status: "unhealthy", Detail: "jenkins_unavailable", CheckedAt: now.UTC().Format(time.RFC3339Nano)}
	var job struct {
		Buildable *bool `json:"buildable"`
	}
	if err == nil && status == 200 && json.Unmarshal(raw, &job) == nil && job.Buildable != nil {
		if *job.Buildable {
			result.Status = "healthy"
			result.Detail = "job_available"
		} else {
			result.Detail = "job_disabled"
		}
	}
	a.probedAt, a.probeResult = now, result
	return result
}
