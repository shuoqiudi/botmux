package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skrashevich/botmux/internal/models"
	"github.com/skrashevich/botmux/internal/store"
)

// AdapterConfig comes from a mounted secret file, never a management API.
// Credentials are excluded even if a caller accidentally serializes config.
type AdapterConfig struct {
	JobURL         string        `json:"-"`
	Username       string        `json:"-"`
	APIToken       string        `json:"-"`
	PollInterval   time.Duration `json:"-"`
	QueueTimeout   time.Duration `json:"-"`
	BuildTimeout   time.Duration `json:"-"`
	ReplyTimeout   time.Duration `json:"-"`
	RequestTimeout time.Duration `json:"-"`
	MaxFailures    int           `json:"-"`
}

type Adapter struct {
	store       *store.Store
	outbound    *Service
	config      AdapterConfig
	jenkins     *jenkinsClient
	fingerprint string
	probeMu     sync.Mutex
	probedAt    time.Time
	probeResult models.GatewayComponentHealth
}

func NewAdapter(repository *store.Store, outbound *Service, config AdapterConfig, client *http.Client) (*Adapter, error) {
	if repository == nil || outbound == nil {
		return nil, errors.New("adapter_dependencies_missing")
	}
	config, err := normalizeAdapterConfig(config)
	if err != nil {
		return nil, err
	}
	jenkins, err := newJenkinsClient(config, client)
	if err != nil {
		return nil, err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(jenkins.jobURL)))
	return &Adapter{store: repository, outbound: outbound, config: config, jenkins: jenkins, fingerprint: fingerprint}, nil
}

// Process advances one bounded step. SQLite leases fence worker reclaim; every
// remote submission is preceded by a durable intent. Pending is not a failure.
func (a *Adapter) Process(ctx context.Context, d *models.InboundDelivery) (done bool, class string, err error) {
	owner := uuid.NewString()
	state, err := a.store.ClaimAdapterExecution(ctx, d.DeliveryID, owner, a.fingerprint, 4*a.config.RequestTimeout+time.Second)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	defer func() {
		saveErr := a.store.SaveAdapterExecution(ctx, state, owner, true)
		if saveErr != nil {
			done, class, err = false, "", saveErr
		}
	}()
	if state.Stage == "done" {
		return true, state.ErrorClass, nil
	}
	now := time.Now().UnixMilli()
	if now < state.NextPollAt {
		return false, "", nil
	}
	state.NextPollAt = now + a.config.PollInterval.Milliseconds()
	if state.JobFingerprint != a.fingerprint && state.Stage != "terminal" {
		a.terminal(state, "failed", "ADAPTER_CONFIG_CHANGED")
	}
	account, err := a.store.GetBotAccount(d.BotAccountID)
	if err != nil {
		return false, "", err
	}
	request, message, parseErr := parseManagementRequest(d, account.Username)
	switch state.Stage {
	case "new":
		if parseErr != nil {
			a.terminal(state, "failed", "INVALID_REQUEST")
			break
		}
		if request == nil {
			state.Stage = "done"
			return true, "", nil
		}
		a.advance(state, "acknowledging")
	case "acknowledging":
		ready, failed, err := a.reply(ctx, d, message, "ack", "Request accepted; execution pending. Request: "+d.DeliveryID)
		if err != nil {
			return false, "", err
		}
		if failed || a.expired(state, a.config.ReplyTimeout) {
			state.Stage = "done"
			state.ErrorClass = "adapter_ack_failed"
			return true, state.ErrorClass, nil
		}
		if ready {
			a.advance(state, "ready")
		}
	case "ready":
		if request == nil || parseErr != nil {
			a.terminal(state, "failed", "INVALID_REQUEST")
			break
		}
		a.advance(state, "triggering")
		// Commit before POST. After a crash only reconciliation is permitted.
		if err := a.store.SaveAdapterExecution(ctx, state, owner, false); err != nil {
			return false, "", err
		}
		queueID, rejected, err := a.jenkins.trigger(ctx, request)
		if rejected {
			a.terminal(state, "failed", "JENKINS_TRIGGER_FAILED")
		} else if err == nil {
			state.QueueID = queueID
			a.advance(state, "queued")
		}
	case "triggering":
		if a.expired(state, a.config.QueueTimeout) {
			a.terminal(state, "timeout", "JENKINS_TRIGGER_UNCERTAIN")
			break
		}
		queue, build, err := a.jenkins.find(ctx, d.DeliveryID, state.QueueID)
		if err != nil {
			a.networkFailure(state, err)
			break
		}
		state.Failures = 0
		if build > 0 {
			state.BuildNumber = build
			a.advance(state, "building")
		} else if queue > 0 {
			state.QueueID = queue
			a.advance(state, "queued")
		}
	case "queued":
		if a.expired(state, a.config.QueueTimeout) {
			a.terminal(state, "timeout", "JENKINS_QUEUE_TIMEOUT")
			break
		}
		build, cancelled, missing, err := a.jenkins.queue(ctx, state.QueueID)
		if err != nil {
			a.networkFailure(state, err)
			break
		}
		if cancelled {
			a.terminal(state, "failed", "JENKINS_TRIGGER_FAILED")
			break
		}
		if missing {
			_, build, err = a.jenkins.find(ctx, d.DeliveryID, state.QueueID)
			if err != nil {
				a.networkFailure(state, err)
				break
			}
		}
		state.Failures = 0
		if build > 0 {
			state.BuildNumber = build
			a.advance(state, "building")
		}
	case "building":
		if a.expired(state, a.config.BuildTimeout) {
			a.terminal(state, "timeout", "JENKINS_BUILD_TIMEOUT")
			break
		}
		building, result, err := a.jenkins.build(ctx, state.BuildNumber)
		if err != nil {
			a.networkFailure(state, err)
			break
		}
		state.Failures = 0
		if !building {
			state.BuildResult = result
			a.advance(state, "result")
		}
	case "result":
		result, err := a.jenkins.result(ctx, state.BuildNumber, d.DeliveryID)
		if err != nil {
			if errors.Is(err, errResultTooLarge) {
				a.terminal(state, "failed", "RESULT_LIMIT_EXCEEDED")
			} else if errors.Is(err, errResultUnavailable) {
				code := "RESULT_UNAVAILABLE"
				if state.BuildResult != "SUCCESS" {
					code = "JENKINS_BUILD_FAILED"
				}
				a.terminal(state, "failed", code)
			} else {
				a.networkFailure(state, err)
			}
			break
		}
		a.terminal(state, result.Status, result.Code)
		state.ReplyPayload, err = json.Marshal(resultReply(state, message, result))
		if err != nil {
			return false, "", err
		}
	case "terminal":
		if len(state.ReplyPayload) == 0 {
			state.ReplyPayload, err = json.Marshal(resultReply(state, message, nil))
			if err != nil {
				return false, "", err
			}
		}
		ready, failed, err := a.queueReply(ctx, d, "result", state.ReplyPayload)
		if err != nil {
			return false, "", err
		}
		if ready {
			state.Stage = "done"
			return true, state.ErrorClass, nil
		}
		if failed || a.expired(state, a.config.ReplyTimeout) {
			state.Stage = "done"
			state.ErrorClass = "adapter_reply_failed"
			return true, state.ErrorClass, nil
		}
	default:
		a.terminal(state, "failed", "ADAPTER_STATE_INVALID")
	}
	return false, "", nil
}

func (a *Adapter) advance(state *models.AdapterExecution, stage string) {
	state.Stage = stage
	state.StageStartedAt = time.Now().UnixMilli()
	state.Failures = 0
}
func (a *Adapter) expired(state *models.AdapterExecution, limit time.Duration) bool {
	return time.Now().UnixMilli()-state.StageStartedAt >= limit.Milliseconds()
}
func (a *Adapter) terminal(state *models.AdapterExecution, status, code string) {
	a.advance(state, "terminal")
	state.ResultStatus = status
	state.ResultCode = code
	state.ErrorClass = strings.ToLower(code)
}
func (a *Adapter) networkFailure(state *models.AdapterExecution, err error) {
	state.Failures++
	state.ErrorClass = "jenkins_unavailable"
	if state.Failures >= a.config.MaxFailures {
		a.terminal(state, "failed", "JENKINS_UNAVAILABLE")
	}
}

func (a *Adapter) reply(ctx context.Context, d *models.InboundDelivery, message adapterMessage, phase, text string) (bool, bool, error) {
	send := &SendMessage{Text: text, MessageThreadID: message.ThreadID}
	if message.MessageID > 0 {
		send.ReplyParameters = &ReplyParameters{MessageID: message.MessageID, AllowSendingWithoutReply: true}
	}
	raw, _ := json.Marshal(outboundEnvelope{Kind: "message", Message: send})
	return a.queueReply(ctx, d, phase, raw)
}

func (a *Adapter) queueReply(ctx context.Context, d *models.InboundDelivery, phase string, raw []byte) (bool, bool, error) {
	reply, err := a.store.CreateAdapterReply(ctx, d.DeliveryID, phase, raw)
	if err != nil {
		return false, false, err
	}
	switch reply.Status {
	case models.GatewayDeliverySucceeded:
		return true, false, nil
	case models.GatewayDeliveryDeadLettered, models.GatewayDeliveryReconciling:
		return false, true, nil
	}
	if _, err := a.outbound.queue.Append(ctx, reply.ID); err != nil {
		return false, false, err
	}
	if err := a.store.MarkGatewayDeliveryAccepted(ctx, reply.ID); err != nil {
		return false, false, err
	}
	return false, false, nil
}
