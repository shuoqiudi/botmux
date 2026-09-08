package models

// AdapterExecution is correlation-only operational state. It never contains
// command arguments, Telegram addresses, console output or credentials.
type AdapterExecution struct {
	DeliveryID     string `json:"delivery_id"`
	Stage          string `json:"stage"`
	QueueID        int64  `json:"queue_id,omitempty"`
	BuildResult    string `json:"build_result,omitempty"`
	BuildNumber    int64  `json:"build_number,omitempty"`
	StartedAt      int64  `json:"started_at"`
	StageStartedAt int64  `json:"stage_started_at"`
	NextPollAt     int64  `json:"next_poll_at,omitempty"`
	Failures       int    `json:"failures,omitempty"`
	ErrorClass     string `json:"error_class,omitempty"`
	ResultStatus   string `json:"result_status,omitempty"`
	ResultCode     string `json:"result_code,omitempty"`
	JobFingerprint string `json:"-"`
}
