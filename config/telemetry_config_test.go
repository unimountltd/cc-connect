package config

import "testing"

func boolp(v bool) *bool { return &v }

func TestTelemetryConfig_OffWithoutAPIKey(t *testing.T) {
	cases := []struct {
		name string
		cfg  TelemetryConfig
		want bool
	}{
		{"empty config is off", TelemetryConfig{}, false},
		{"legacy enabled=true without key is still off", TelemetryConfig{Enabled: boolp(true)}, false},
		{"disabled=false without key is still off", TelemetryConfig{Disabled: boolp(false)}, false},
		{"api_key alone turns it on", TelemetryConfig{APIKey: "phc_test"}, true},
		{"disabled=true wins over api_key", TelemetryConfig{APIKey: "phc_test", Disabled: boolp(true)}, false},
		{"legacy enabled=false wins over api_key", TelemetryConfig{APIKey: "phc_test", Enabled: boolp(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.TelemetryEnabled(); got != tc.want {
				t.Fatalf("TelemetryEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTelemetryConfig_NoBuiltInKey(t *testing.T) {
	if got := (TelemetryConfig{}).EffectiveAPIKey(); got != "" {
		t.Fatalf("EffectiveAPIKey() must be empty without config, got %q", got)
	}
	if got := (TelemetryConfig{APIKey: "phc_mine"}).EffectiveAPIKey(); got != "phc_mine" {
		t.Fatalf("EffectiveAPIKey() = %q, want configured key", got)
	}
}
