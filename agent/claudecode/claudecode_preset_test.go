package claudecode

import "testing"

func TestAvailablePresets_DefaultsWhenUnconfigured(t *testing.T) {
	a := &Agent{}
	got := a.AvailablePresets()
	if len(got) != 2 {
		t.Fatalf("default presets = %d, want 2", len(got))
	}
	if got[0].Name != "fable" || got[0].Model != "fable" || got[0].Effort != "high" {
		t.Fatalf("preset[0] = %#v, want fable/fable/high", got[0])
	}
	if got[1].Name != "opus" || got[1].Model != "opus" || got[1].Effort != "high" {
		t.Fatalf("preset[1] = %#v, want opus/opus/high", got[1])
	}
}

func TestParsePresets_FromTOMLShape(t *testing.T) {
	raw := []any{
		map[string]any{"name": "fast", "model": "haiku", "effort": "low", "desc": "quick"},
		map[string]any{"name": "deep", "model": "opus", "effort": "MAX"},
		map[string]any{"name": "", "model": "x"}, // skipped: no name
		map[string]any{"name": "y", "model": ""}, // skipped: no model
	}
	got := parsePresets(raw)
	if len(got) != 2 {
		t.Fatalf("parsed presets = %d, want 2", len(got))
	}
	if got[0].Name != "fast" || got[0].Model != "haiku" || got[0].Effort != "low" || got[0].Desc != "quick" {
		t.Fatalf("preset[0] = %#v", got[0])
	}
	if got[1].Name != "deep" || got[1].Model != "opus" || got[1].Effort != "MAX" {
		t.Fatalf("preset[1] = %#v", got[1])
	}
}

func TestAvailablePresets_ConfiguredNormalizesEffort(t *testing.T) {
	a := &Agent{presets: parsePresets([]any{
		map[string]any{"name": "deep", "model": "opus", "effort": "HIGH"},
		map[string]any{"name": "weird", "model": "sonnet", "effort": "bogus"},
	})}
	got := a.AvailablePresets()
	if len(got) != 2 {
		t.Fatalf("presets = %d, want 2", len(got))
	}
	if got[0].Effort != "high" {
		t.Fatalf("effort not normalized: %q, want high", got[0].Effort)
	}
	if got[1].Effort != "" {
		t.Fatalf("bogus effort should normalize to empty, got %q", got[1].Effort)
	}
}
