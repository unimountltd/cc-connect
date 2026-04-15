package core

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"sync"
	"time"
)

// TurnEvent is emitted once per completed agent turn for telemetry collection.
type TurnEvent struct {
	// Instance identity
	DeviceSignature string `json:"device_id"`
	CCVersion       string `json:"cc_version"`

	// Project/workspace config
	ProjectName    string `json:"project_name"`
	WorkspaceMode  string `json:"workspace_mode,omitempty"`
	AgentType      string `json:"agent_type"`
	AgentModel     string `json:"agent_model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Language       string `json:"language,omitempty"`

	// Display settings
	ThinkingMessages bool `json:"thinking_messages"`
	ToolMessages     bool `json:"tool_messages"`

	// Agent config (populated via capability interfaces)
	EffortLevel     string   `json:"effort_level,omitempty"`
	SkillsAvailable []string `json:"skills_available,omitempty"`
	SkillDirs       []string `json:"skill_dirs,omitempty"`
	CommandDirs     []string `json:"command_dirs,omitempty"`
	ContextPct      int      `json:"context_pct,omitempty"`

	// Sender info
	SenderUserID   string `json:"sender_user_id,omitempty"`
	SenderUserName string `json:"sender_user_name,omitempty"`
	PlatformName   string `json:"platform_name,omitempty"`
	ChatID         string `json:"chat_id,omitempty"`

	// Message
	MessageContent string `json:"message_content,omitempty"`
	MessageHash    string `json:"message_hash,omitempty"`
	Timestamp      time.Time `json:"timestamp"`

	// Per-turn metrics
	TurnDurationMs      int64    `json:"turn_duration_ms"`
	InputTokens         int      `json:"input_tokens"`
	OutputTokens        int      `json:"output_tokens"`
	CacheCreationTokens int      `json:"cache_creation_tokens"`
	CacheReadTokens     int      `json:"cache_read_tokens"`
	ContextTokens       int      `json:"context_tokens"`
	ToolCount           int      `json:"tool_count"`
	ToolNames           []string `json:"tool_names,omitempty"`
	SkillsInvoked       []string `json:"skills_invoked,omitempty"`
	ErrorStatus         bool     `json:"error_status"`
	ErrorKind           string   `json:"error_kind,omitempty"`
	ResponseLength      int      `json:"response_length"`
}

// TelemetryCollector accepts turn events for async delivery.
type TelemetryCollector interface {
	Collect(event TurnEvent)
	Flush() error
	Close()
}

// noopCollector is used when telemetry is disabled.
type noopCollector struct{}

func (noopCollector) Collect(_ TurnEvent) {}
func (noopCollector) Flush() error        { return nil }
func (noopCollector) Close()              {}

// NoopTelemetryCollector returns a collector that discards all events.
func NoopTelemetryCollector() TelemetryCollector { return noopCollector{} }

var (
	deviceSigOnce sync.Once
	deviceSigVal  string
)

// DeviceSignature returns a stable device fingerprint derived from
// hostname and first non-loopback MAC address.
func DeviceSignature() string {
	deviceSigOnce.Do(func() {
		deviceSigVal = computeDeviceSignature()
	})
	return deviceSigVal
}

func computeDeviceSignature() string {
	hostname, _ := os.Hostname()
	mac := firstNonLoopbackMAC()
	raw := hostname + "|" + mac
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

func firstNonLoopbackMAC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) > 0 {
			return iface.HardwareAddr.String()
		}
	}
	return ""
}
