package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// LoadAdapterConfig reads a mounted secret without exposing its contents in
// errors. No environment-variable contract or public Adapter port is needed.
func LoadAdapterConfig(path string) (AdapterConfig, error) {
	invalid := errors.New("adapter_config_invalid")
	file, err := os.Open(path)
	if err != nil {
		return AdapterConfig{}, errors.New("adapter_config_unreadable")
	}
	defer file.Close()
	var input struct {
		JobURL   string `json:"job_url"`
		Username string `json:"username"`
		APIToken string `json:"api_token"`
		Poll     int    `json:"poll_interval_seconds"`
		Queue    int    `json:"queue_timeout_seconds"`
		Build    int    `json:"build_timeout_seconds"`
		Reply    int    `json:"reply_timeout_seconds"`
		Request  int    `json:"request_timeout_seconds"`
		Failures int    `json:"max_failures"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		return AdapterConfig{}, invalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return AdapterConfig{}, invalid
	}
	for _, seconds := range []int{input.Poll, input.Queue, input.Build, input.Reply, input.Request} {
		if seconds < 0 || seconds > 86400 {
			return AdapterConfig{}, invalid
		}
	}
	config, err := normalizeAdapterConfig(AdapterConfig{JobURL: input.JobURL, Username: input.Username, APIToken: input.APIToken,
		PollInterval: time.Duration(input.Poll) * time.Second, QueueTimeout: time.Duration(input.Queue) * time.Second,
		BuildTimeout: time.Duration(input.Build) * time.Second, ReplyTimeout: time.Duration(input.Reply) * time.Second,
		RequestTimeout: time.Duration(input.Request) * time.Second, MaxFailures: input.Failures})
	if err != nil {
		return AdapterConfig{}, invalid
	}
	if _, err := newJenkinsClient(config, nil); err != nil {
		return AdapterConfig{}, invalid
	}
	return config, nil
}

func normalizeAdapterConfig(config AdapterConfig) (AdapterConfig, error) {
	for _, setting := range []struct {
		value             *time.Duration
		fallback, maximum time.Duration
	}{
		{&config.PollInterval, 2 * time.Second, time.Minute},
		{&config.QueueTimeout, 5 * time.Minute, time.Hour},
		{&config.BuildTimeout, 30 * time.Minute, 24 * time.Hour},
		{&config.ReplyTimeout, 2 * time.Minute, time.Hour},
		{&config.RequestTimeout, 10 * time.Second, 30 * time.Second},
	} {
		if *setting.value < 0 || *setting.value > setting.maximum {
			return AdapterConfig{}, errors.New("adapter_config_invalid")
		}
		if *setting.value == 0 {
			*setting.value = setting.fallback
		}
	}
	if config.MaxFailures < 0 || config.MaxFailures > 20 {
		return AdapterConfig{}, errors.New("adapter_config_invalid")
	}
	if config.MaxFailures == 0 {
		config.MaxFailures = 5
	}
	return config, nil
}
