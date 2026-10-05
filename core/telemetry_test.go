package core

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// --- noopCollector ---

func TestNoopCollector(t *testing.T) {
	c := NoopTelemetryCollector()
	c.Collect(TurnEvent{ProjectName: "test"})
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// --- DeviceSignature ---

func TestDeviceSignatureStable(t *testing.T) {
	a := DeviceSignature()
	b := DeviceSignature()
	if a != b {
		t.Fatalf("DeviceSignature not stable: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Fatalf("DeviceSignature length = %d, want 16", len(a))
	}
}

// --- PostHogCollector ---

func TestPostHogCollectorSendsEvent(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		json.Unmarshal(body, &payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewPostHogCollector("phc_test123", srv.URL+"/capture/", false)
	c.Collect(TurnEvent{
		DeviceSignature: "abc123",
		CCVersion:       "v1.0.0",
		ProjectName:     "myproject",
		PlatformName:    "slack",
		InputTokens:     100,
		OutputTokens:    50,
		Timestamp:       time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC),
	})

	c.Flush()
	c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}

	payload := received[0]
	if payload["api_key"] != "phc_test123" {
		t.Errorf("api_key = %v", payload["api_key"])
	}
	if payload["event"] != "turn_complete" {
		t.Errorf("event = %v", payload["event"])
	}
	if payload["distinct_id"] != "abc123" {
		t.Errorf("distinct_id = %v", payload["distinct_id"])
	}

	props, ok := payload["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("properties not a map")
	}
	if props["project_name"] != "myproject" {
		t.Errorf("project_name = %v", props["project_name"])
	}
	if props["platform_name"] != "slack" {
		t.Errorf("platform_name = %v", props["platform_name"])
	}
}

func TestPostHogCollectorHashContent(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		json.Unmarshal(body, &payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewPostHogCollector("phc_test", srv.URL+"/capture/", true)
	c.Collect(TurnEvent{
		DeviceSignature: "abc",
		MessageContent:  "secret message",
		Timestamp:       time.Now(),
	})

	c.Flush()
	c.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatal("expected 1 event")
	}

	props := received[0]["properties"].(map[string]interface{})
	if mc, ok := props["message_content"]; ok && mc != "" {
		t.Errorf("message_content should be empty when hashed, got %v", mc)
	}
	if props["message_hash"] == "" {
		t.Error("message_hash should be populated")
	}
}

func TestPostHogCollectorDropsOnFullBuffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewPostHogCollector("phc_test", srv.URL+"/capture/", false)

	// Fill the buffer beyond capacity — should not block
	for i := 0; i < posthogBufferSize+10; i++ {
		c.Collect(TurnEvent{DeviceSignature: "test", Timestamp: time.Now()})
	}

	c.Close()
}
