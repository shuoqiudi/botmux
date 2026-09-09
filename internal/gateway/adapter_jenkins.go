package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var errJenkinsUnavailable = errors.New("jenkins_unavailable")
var errResultTooLarge = errors.New("result_limit_exceeded")
var errResultUnavailable = errors.New("result_unavailable")
var jenkinsJobPath = regexp.MustCompile(`^(?:/[A-Za-z0-9_.-]+)*/job/[A-Za-z0-9_.-]+(?:/job/[A-Za-z0-9_.-]+)*$`)
var jenkinsConsoleTimestamp = regexp.MustCompile(`^\[[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z\] `)

type jenkinsClient struct {
	jobURL   string
	rootURL  string
	username string
	token    string
	client   *http.Client
	timeout  time.Duration
}

func newJenkinsClient(config AdapterConfig, client *http.Client) (*jenkinsClient, error) {
	jobURL := strings.TrimRight(config.JobURL, "/")
	parsed, err := url.Parse(jobURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !jenkinsJobPath.MatchString(parsed.Path) || strings.Contains(parsed.Path, "/../") || strings.Contains(parsed.Path, "/./") || strings.HasSuffix(parsed.Path, "/..") || strings.HasSuffix(parsed.Path, "/.") {
		return nil, errors.New("adapter_job_url_invalid")
	}
	if config.Username == "" || config.APIToken == "" || strings.ContainsAny(config.Username, ":\r\n") || strings.ContainsAny(config.APIToken, "\r\n") {
		return nil, errors.New("adapter_credentials_invalid")
	}
	if client == nil {
		client = &http.Client{}
	}
	safeClient := *client
	// Never follow Jenkins redirects with a service credential or POST body.
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &jenkinsClient{jobURL: jobURL, rootURL: parsed.Scheme + "://" + parsed.Host + parsed.Path[:strings.Index(parsed.Path, "/job/")], username: config.Username, token: config.APIToken, client: &safeClient, timeout: config.RequestTimeout}, nil
}

func (j *jenkinsClient) call(ctx context.Context, method, endpoint string, form url.Values, limit int64) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, nil, nil, errJenkinsUnavailable
	}
	req.SetBasicAuth(j.username, j.token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return 0, nil, nil, errJenkinsUnavailable
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(raw)) > limit {
		return resp.StatusCode, resp.Header, nil, errResultTooLarge
	}
	if err != nil {
		return resp.StatusCode, resp.Header, nil, errJenkinsUnavailable
	}
	return resp.StatusCode, resp.Header, raw, nil
}

func (j *jenkinsClient) trigger(ctx context.Context, request *managementRequest) (int64, bool, error) {
	raw, _ := json.Marshal(request)
	status, headers, _, err := j.call(ctx, http.MethodPost, j.jobURL+"/buildWithParameters", url.Values{
		"IT_MANAGE_REQUEST_JSON_B64": {base64.StdEncoding.EncodeToString(raw)},
		"IT_MANAGE_REQUEST_LABEL":    {"gateway:" + request.Identity.RequestID},
	}, 64*1024)
	if status >= 400 && status < 500 {
		return 0, true, nil
	}
	if err != nil || (status != 201 && status != 202) {
		return 0, false, errJenkinsUnavailable
	}
	queue, ok := j.referenceNumber(headers.Get("Location"), j.rootURL+"/queue/item/")
	if !ok {
		return 0, false, errJenkinsUnavailable
	}
	return queue, false, nil
}

// References from Jenkins must have the configured origin and exact resource
// shape. We persist only numbers and construct all later URLs ourselves.
func (j *jenkinsClient) referenceNumber(value, prefix string) (int64, bool) {
	base, _ := url.Parse(j.rootURL + "/")
	ref, err := url.Parse(value)
	if err != nil || ref.User != nil || ref.RawQuery != "" || ref.Fragment != "" {
		return 0, false
	}
	absolute := base.ResolveReference(ref).String()
	if !strings.HasPrefix(absolute, prefix) || !strings.HasSuffix(absolute, "/") {
		return 0, false
	}
	number := strings.TrimSuffix(strings.TrimPrefix(absolute, prefix), "/")
	for _, ch := range number {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	return n, err == nil && n > 0
}

func (j *jenkinsClient) queue(ctx context.Context, id int64) (build int64, cancelled, missing bool, err error) {
	status, _, raw, err := j.call(ctx, http.MethodGet, fmt.Sprintf("%s/queue/item/%d/api/json", j.rootURL, id), nil, 1<<20)
	if err != nil {
		return 0, false, false, err
	}
	if status == 404 {
		return 0, false, true, nil
	}
	var result struct {
		Cancelled  bool `json:"cancelled"`
		Executable *struct {
			Number int64  `json:"number"`
			URL    string `json:"url"`
		} `json:"executable"`
	}
	if status != 200 || json.Unmarshal(raw, &result) != nil {
		return 0, false, false, errJenkinsUnavailable
	}
	if result.Executable != nil {
		number, ok := j.referenceNumber(result.Executable.URL, j.jobURL+"/")
		if !ok || number != result.Executable.Number {
			return 0, false, false, errJenkinsUnavailable
		}
		return number, false, false, nil
	}
	return 0, result.Cancelled, false, nil
}

type jenkinsAction struct {
	Parameters []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"parameters"`
}

func matchesManagementRequest(actions []jenkinsAction, id string) bool {
	label, identity := false, false
	for _, action := range actions {
		for _, parameter := range action.Parameters {
			if parameter.Name == "IT_MANAGE_REQUEST_LABEL" && parameter.Value == "gateway:"+id {
				label = true
			}
			if parameter.Name == "IT_MANAGE_REQUEST_JSON_B64" {
				raw, err := base64.StdEncoding.DecodeString(parameter.Value)
				var request managementRequest
				if err == nil && json.Unmarshal(raw, &request) == nil && request.Schema == managementRequestSchema && request.Identity.RequestID == id && request.Identity.DeliveryID == id {
					identity = true
				}
			}
		}
	}
	return label && identity
}

// Bounded recovery never resubmits an ambiguous POST. Older entries outside
// the scan window become an explicit uncertain outcome instead of duplicates.
func (j *jenkinsClient) find(ctx context.Context, id string, queueID int64) (int64, int64, error) {
	tree := url.QueryEscape("items[id,task[url],actions[parameters[name,value]]]")
	status, _, raw, err := j.call(ctx, http.MethodGet, j.rootURL+"/queue/api/json?tree="+tree, nil, 4<<20)
	var queue struct {
		Items []struct {
			ID   int64 `json:"id"`
			Task struct {
				URL string `json:"url"`
			} `json:"task"`
			Actions []jenkinsAction `json:"actions"`
		} `json:"items"`
	}
	if err != nil || status != 200 || json.Unmarshal(raw, &queue) != nil {
		return 0, 0, errJenkinsUnavailable
	}
	for _, item := range queue.Items {
		if item.ID > 0 && strings.TrimRight(item.Task.URL, "/") == j.jobURL && matchesManagementRequest(item.Actions, id) {
			return item.ID, 0, nil
		}
	}
	tree = url.QueryEscape("builds[number,queueId,actions[parameters[name,value]]]{0,100}")
	status, _, raw, err = j.call(ctx, http.MethodGet, j.jobURL+"/api/json?tree="+tree, nil, 4<<20)
	var builds struct {
		Builds []struct {
			Number  int64           `json:"number"`
			QueueID int64           `json:"queueId"`
			Actions []jenkinsAction `json:"actions"`
		} `json:"builds"`
	}
	if err != nil || status != 200 || json.Unmarshal(raw, &builds) != nil {
		return 0, 0, errJenkinsUnavailable
	}
	for _, build := range builds.Builds {
		if build.Number > 0 && matchesManagementRequest(build.Actions, id) && (queueID == 0 || queueID == build.QueueID) {
			return 0, build.Number, nil
		}
	}
	return 0, 0, nil
}

func (j *jenkinsClient) build(ctx context.Context, number int64) (bool, string, error) {
	status, _, raw, err := j.call(ctx, http.MethodGet, fmt.Sprintf("%s/%d/api/json?tree=building,result", j.jobURL, number), nil, 1<<20)
	var build struct {
		Building *bool  `json:"building"`
		Result   string `json:"result"`
	}
	if err != nil || status != 200 || json.Unmarshal(raw, &build) != nil || build.Building == nil {
		return false, "", errJenkinsUnavailable
	}
	if !*build.Building {
		switch build.Result {
		case "SUCCESS", "FAILURE", "ABORTED", "UNSTABLE", "NOT_BUILT":
		default:
			return false, "", errJenkinsUnavailable
		}
	}
	return *build.Building, build.Result, nil
}

func (j *jenkinsClient) result(ctx context.Context, number int64, id string) (*managementResult, error) {
	status, _, raw, err := j.call(ctx, http.MethodGet, fmt.Sprintf("%s/%d/consoleText", j.jobURL, number), nil, 8<<20)
	if err != nil || status != 200 {
		if errors.Is(err, errResultTooLarge) {
			return nil, err
		}
		return nil, errJenkinsUnavailable
	}
	const begin = "IT_MANAGE_MANAGEMENT_RESULT_BEGIN:"
	const end = ":IT_MANAGE_MANAGEMENT_RESULT_END"
	var found *managementResult
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		// Jenkins Timestamper decorates consoleText lines. Strip only its
		// known prefix; arbitrary console text must not become a result frame.
		line = jenkinsConsoleTimestamp.ReplaceAllString(line, "")
		if !strings.HasPrefix(line, begin) || !strings.HasSuffix(line, end) {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(line, begin), end))
		var result managementResult
		if err != nil || json.Unmarshal(decoded, &result) != nil || result.Schema != managementResultSchema || result.RequestID != id || result.DeliveryID != id {
			continue
		}
		valid := false
		switch result.Status {
		case "succeeded":
			valid = result.Code == ""
		case "business_failed":
			valid = result.Code == "BUSINESS_FAILED"
		case "timeout":
			valid = result.Code == "BUSINESS_TIMEOUT"
		case "failed":
			valid = result.Code == "RESULT_UNAVAILABLE" || result.Code == "JENKINS_BUILD_FAILED" || result.Code == "JENKINS_TRIGGER_FAILED"
		}
		if valid {
			found = &result
		}
	}
	if found == nil {
		return nil, errResultUnavailable
	}
	return found, nil
}
