package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// API is the subset of the Miabi API the autoscaler uses (an interface so tests can fake it).
type API interface {
	App(ctx context.Context, id int) (App, error)
	AppStatus(ctx context.Context, id int) (Status, error)
	Analytics(ctx context.Context, id int, rng string) (Summary, error)
	Scale(ctx context.Context, id, replicas int) error
}

type App struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Replicas    int    `json:"replicas"`
	RuntimeKind string `json:"runtime_kind"`
}

type Status struct {
	Running             bool `json:"running"`
	ServiceReplicas     int  `json:"service_replicas"`
	ServiceRunningTasks int  `json:"service_running_tasks"`
}

type Bucket struct {
	T         time.Time `json:"t"`
	Requests  float64   `json:"requests"`
	Errors5xx float64   `json:"errors_5xx"`
	P95Ms     float64   `json:"p95_latency_ms"`
}

type Summary struct {
	Granularity string   `json:"granularity"`
	Series      []Bucket `json:"series"`
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("miabi API: HTTP %d: %s", e.Status, e.Message) }

type Client struct {
	base      string
	workspace string
	token     string
	hc        *http.Client
}

func NewClient(baseURL, workspace, token string) *Client {
	return &Client{
		base:      strings.TrimRight(baseURL, "/"),
		workspace: url.PathEscape(workspace),
		token:     token,
		hc:        &http.Client{Timeout: 20 * time.Second},
	}
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v1/workspaces/"+c.workspace+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		return &APIError{Status: resp.StatusCode, Message: "resposta nao-JSON"}
	}
	if resp.StatusCode >= 300 || !env.Success {
		msg := http.StatusText(resp.StatusCode)
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

func (c *Client) App(ctx context.Context, id int) (a App, err error) {
	err = c.do(ctx, http.MethodGet, fmt.Sprintf("/apps/%d", id), nil, &a)
	return
}

func (c *Client) AppStatus(ctx context.Context, id int) (s Status, err error) {
	err = c.do(ctx, http.MethodGet, fmt.Sprintf("/apps/%d/status", id), nil, &s)
	return
}

func (c *Client) Analytics(ctx context.Context, id int, rng string) (s Summary, err error) {
	q := url.Values{"range": {rng}, "app": {fmt.Sprint(id)}}
	err = c.do(ctx, http.MethodGet, "/analytics/summary?"+q.Encode(), nil, &s)
	return
}

func (c *Client) Scale(ctx context.Context, id, replicas int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/apps/%d/scale", id), map[string]int{"replicas": replicas}, nil)
}
