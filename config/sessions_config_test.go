package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

func loadFromString(t *testing.T, toml string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

func TestEffectiveMaxLiveSessions(t *testing.T) {
	if got := EffectiveMaxLiveSessions(&Config{}); got != DefaultMaxLiveSessions {
		t.Fatalf("default = %d, want %d", got, DefaultMaxLiveSessions)
	}
	if got := EffectiveMaxLiveSessions(&Config{Sessions: SessionsConfig{MaxLive: intp(0)}}); got != 0 {
		t.Fatalf("explicit 0 must mean unlimited, got %d", got)
	}
	if got := EffectiveMaxLiveSessions(&Config{Sessions: SessionsConfig{MaxLive: intp(7)}}); got != 7 {
		t.Fatalf("explicit 7, got %d", got)
	}
	if got := EffectiveMaxLiveSessions(nil); got != DefaultMaxLiveSessions {
		t.Fatalf("nil cfg = %d, want default", got)
	}
}

func TestEffectiveAgentSessionIdleTimeoutMins(t *testing.T) {
	cfg := &Config{}
	if got := EffectiveAgentSessionIdleTimeoutMins(cfg, &ProjectConfig{}); got != DefaultAgentSessionIdleTimeoutMins {
		t.Fatalf("default = %d, want %d", got, DefaultAgentSessionIdleTimeoutMins)
	}
	cfg.Sessions.IdleTimeoutMins = intp(0)
	if got := EffectiveAgentSessionIdleTimeoutMins(cfg, &ProjectConfig{}); got != 0 {
		t.Fatalf("global 0 must mean never, got %d", got)
	}
	cfg.Sessions.IdleTimeoutMins = intp(90)
	if got := EffectiveAgentSessionIdleTimeoutMins(cfg, &ProjectConfig{}); got != 90 {
		t.Fatalf("global 90, got %d", got)
	}
	proj := &ProjectConfig{AgentSessionIdleTimeoutMins: intp(30)}
	if got := EffectiveAgentSessionIdleTimeoutMins(cfg, proj); got != 30 {
		t.Fatalf("project override 30 beats global, got %d", got)
	}
	proj.AgentSessionIdleTimeoutMins = intp(0)
	if got := EffectiveAgentSessionIdleTimeoutMins(cfg, proj); got != 0 {
		t.Fatalf("project 0 must disable even with a global value, got %d", got)
	}
}

func TestSessionsConfig_ParsesAndValidates(t *testing.T) {
	cfg, err := loadFromString(t, `
[sessions]
max_live = 2
idle_timeout_mins = 60

[[projects]]
name = "p"
max_live_agent_sessions = 1
[projects.agent]
type = "claudecode"
[[projects.platforms]]
type = "telegram"
token = "x"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := EffectiveMaxLiveSessions(cfg); got != 2 {
		t.Fatalf("max_live = %d, want 2", got)
	}
	if got := EffectiveAgentSessionIdleTimeoutMins(cfg, &cfg.Projects[0]); got != 60 {
		t.Fatalf("idle = %d, want 60", got)
	}
	if cfg.Projects[0].MaxLiveAgentSessions == nil || *cfg.Projects[0].MaxLiveAgentSessions != 1 {
		t.Fatalf("project max_live_agent_sessions not parsed: %+v", cfg.Projects[0].MaxLiveAgentSessions)
	}

	_, err = loadFromString(t, `
[[projects]]
name = "p"
max_live_agent_sessions = -1
[projects.agent]
type = "claudecode"
[[projects.platforms]]
type = "telegram"
token = "x"
`)
	if err == nil || !strings.Contains(err.Error(), "max_live_agent_sessions must be >= 0") {
		t.Fatalf("expected validation error for negative cap, got %v", err)
	}
}
