package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	posthogDefaultEndpoint = "https://eu.i.posthog.com/capture/"
	posthogEventName       = "turn_complete"
	posthogBufferSize      = 256
	posthogHTTPTimeout     = 10 * time.Second
)

// PostHogCollector sends TurnEvents to PostHog via the /capture HTTP API.
type PostHogCollector struct {
	apiKey      string
	endpoint    string
	hashContent bool
	ch          chan TurnEvent
	done        chan struct{}
	wg          sync.WaitGroup
	httpClient  *http.Client
}

// NewPostHogCollector creates a collector that asynchronously delivers events
// to the PostHog /capture endpoint. Call Close() on shutdown.
func NewPostHogCollector(apiKey, endpoint string, hashContent bool) *PostHogCollector {
	if endpoint == "" {
		endpoint = posthogDefaultEndpoint
	}
	c := &PostHogCollector{
		apiKey:      apiKey,
		endpoint:    endpoint,
		hashContent: hashContent,
		ch:          make(chan TurnEvent, posthogBufferSize),
		done:        make(chan struct{}),
		httpClient:  &http.Client{Timeout: posthogHTTPTimeout},
	}
	c.wg.Add(1)
	go c.worker()
	return c
}

// Collect enqueues an event for async delivery. Non-blocking; drops
// the event if the buffer is full.
func (c *PostHogCollector) Collect(event TurnEvent) {
	if c.hashContent && event.MessageContent != "" {
		sum := sha256.Sum256([]byte(event.MessageContent))
		event.MessageHash = hex.EncodeToString(sum[:])
		event.MessageContent = ""
	}
	select {
	case c.ch <- event:
	default:
		slog.Debug("telemetry: event dropped, buffer full")
	}
}

// Flush drains pending events synchronously. Returns after all buffered
// events have been sent (or failed).
func (c *PostHogCollector) Flush() error {
	for {
		select {
		case ev := <-c.ch:
			c.postEvent(ev)
		default:
			return nil
		}
	}
}

// Close signals the worker to stop and waits for it to finish.
func (c *PostHogCollector) Close() {
	close(c.done)
	c.wg.Wait()
}

func (c *PostHogCollector) worker() {
	defer c.wg.Done()
	for {
		select {
		case ev := <-c.ch:
			c.postEvent(ev)
		case <-c.done:
			// drain remaining events
			for {
				select {
				case ev := <-c.ch:
					c.postEvent(ev)
				default:
					return
				}
			}
		}
	}
}

type posthogCapture struct {
	APIKey     string    `json:"api_key"`
	Event      string    `json:"event"`
	DistinctID string    `json:"distinct_id"`
	Properties TurnEvent `json:"properties"`
	Timestamp  string    `json:"timestamp"`
}

func (c *PostHogCollector) postEvent(ev TurnEvent) {
	payload := posthogCapture{
		APIKey:     c.apiKey,
		Event:      posthogEventName,
		DistinctID: ev.DeviceSignature,
		Properties: ev,
		Timestamp:  ev.Timestamp.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("telemetry: marshal error", "error", err)
		return
	}

	resp, err := c.httpClient.Post(c.endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Debug("telemetry: post failed", "error", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("telemetry: posthog returned non-2xx", "status", resp.StatusCode)
	}
}

// PostHogQueryClient queries the PostHog HogQL API for usage analytics.
type PostHogQueryClient struct {
	PersonalAPIKey string
	ProjectID      string
	BaseURL        string
	httpClient     *http.Client
}

// NewPostHogQueryClient creates a client for querying PostHog HogQL.
func NewPostHogQueryClient(personalAPIKey, projectID, baseURL string) *PostHogQueryClient {
	if baseURL == "" {
		baseURL = "https://eu.posthog.com"
	}
	return &PostHogQueryClient{
		PersonalAPIKey: personalAPIKey,
		ProjectID:      projectID,
		BaseURL:        baseURL,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// QueryResult holds a HogQL query response.
type QueryResult struct {
	Columns []string        `json:"columns"`
	Results [][]interface{} `json:"results"`
}

// Query executes a HogQL query and returns the result.
func (c *PostHogQueryClient) Query(hogql string) (*QueryResult, error) {
	reqBody := map[string]interface{}{
		"query": map[string]interface{}{
			"kind":  "HogQLQuery",
			"query": hogql,
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("telemetry: query marshal: %w", err)
	}

	url := fmt.Sprintf("%s/api/projects/%s/query/", c.BaseURL, c.ProjectID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("telemetry: query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.PersonalAPIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telemetry: query failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("telemetry: query returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Columns []string        `json:"columns"`
		Results [][]interface{} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("telemetry: decode query response: %w", err)
	}
	return &QueryResult{Columns: result.Columns, Results: result.Results}, nil
}
