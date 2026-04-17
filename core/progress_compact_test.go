package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type suppressTestPlatform struct {
	style string
}

func (s *suppressTestPlatform) Name() string                             { return "test" }
func (s *suppressTestPlatform) Start(MessageHandler) error               { return nil }
func (s *suppressTestPlatform) Reply(context.Context, any, string) error { return nil }
func (s *suppressTestPlatform) Send(context.Context, any, string) error  { return nil }
func (s *suppressTestPlatform) Stop() error                              { return nil }
func (s *suppressTestPlatform) ProgressStyle() string                    { return s.style }

func TestSuppressStandaloneToolResultEvent(t *testing.T) {
	if SuppressStandaloneToolResultEvent(&stubPlatformNoProgress{}) {
		t.Fatal("platform without ProgressStyleProvider should not suppress")
	}
	if !SuppressStandaloneToolResultEvent(&suppressTestPlatform{style: "legacy"}) {
		t.Fatal("legacy ProgressStyleProvider should suppress standalone tool results")
	}
	if SuppressStandaloneToolResultEvent(&suppressTestPlatform{style: "compact"}) {
		t.Fatal("compact should not suppress (writer absorbs tool results)")
	}
	if SuppressStandaloneToolResultEvent(&suppressTestPlatform{style: "card"}) {
		t.Fatal("card should not suppress")
	}
}

// stubPlatformNoProgress is a minimal Platform without ProgressStyleProvider.
type stubPlatformNoProgress struct{}

func (stubPlatformNoProgress) Name() string                             { return "plain" }
func (stubPlatformNoProgress) Start(MessageHandler) error               { return nil }
func (stubPlatformNoProgress) Reply(context.Context, any, string) error { return nil }
func (stubPlatformNoProgress) Send(context.Context, any, string) error  { return nil }
func (stubPlatformNoProgress) Stop() error                              { return nil }

func TestBuildAndParseProgressCardPayload(t *testing.T) {
	payload := BuildProgressCardPayload([]string{" step1 ", "", "step2"}, true)
	if payload == "" {
		t.Fatal("BuildProgressCardPayload returned empty string")
	}
	if !strings.HasPrefix(payload, ProgressCardPayloadPrefix) {
		t.Fatalf("payload = %q, want prefix %q", payload, ProgressCardPayloadPrefix)
	}

	parsed, ok := ParseProgressCardPayload(payload)
	if !ok {
		t.Fatalf("ParseProgressCardPayload should succeed, payload=%q", payload)
	}
	if len(parsed.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(parsed.Entries))
	}
	if parsed.Entries[0] != "step1" || parsed.Entries[1] != "step2" {
		t.Fatalf("entries = %#v, want [step1 step2]", parsed.Entries)
	}
	if !parsed.Truncated {
		t.Fatal("parsed.Truncated = false, want true")
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(parsed.Items))
	}
	if parsed.Items[0].Kind != ProgressEntryInfo || parsed.Items[0].Text != "step1" {
		t.Fatalf("items[0] = %#v, want info/step1", parsed.Items[0])
	}
}

func TestBuildAndParseProgressCardPayloadV2(t *testing.T) {
	payload := BuildProgressCardPayloadV2([]ProgressCardEntry{
		{Kind: ProgressEntryThinking, Text: " plan "},
		{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "pwd"},
	}, false, "Codex", LangChinese, ProgressCardStateRunning)
	if payload == "" {
		t.Fatal("BuildProgressCardPayloadV2 returned empty string")
	}

	parsed, ok := ParseProgressCardPayload(payload)
	if !ok {
		t.Fatalf("ParseProgressCardPayload should succeed, payload=%q", payload)
	}
	if parsed.Version != 2 {
		t.Fatalf("version = %d, want 2", parsed.Version)
	}
	if parsed.Agent != "Codex" {
		t.Fatalf("agent = %q, want Codex", parsed.Agent)
	}
	if parsed.Lang != string(LangChinese) {
		t.Fatalf("lang = %q, want %q", parsed.Lang, LangChinese)
	}
	if parsed.State != ProgressCardStateRunning {
		t.Fatalf("state = %q, want %q", parsed.State, ProgressCardStateRunning)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(parsed.Items))
	}
	if parsed.Items[1].Kind != ProgressEntryToolUse || parsed.Items[1].Tool != "Bash" {
		t.Fatalf("items[1] = %#v, want tool_use/Bash", parsed.Items[1])
	}
}

func TestParseProgressCardPayloadRejectsInvalid(t *testing.T) {
	if _, ok := ParseProgressCardPayload("plain text"); ok {
		t.Fatal("expected parse failure for plain text")
	}
	if _, ok := ParseProgressCardPayload(ProgressCardPayloadPrefix + "{not-json"); ok {
		t.Fatal("expected parse failure for invalid json")
	}
	if _, ok := ParseProgressCardPayload(ProgressCardPayloadPrefix + `{"entries":[]}`); ok {
		t.Fatal("expected parse failure for empty entries")
	}
}

func TestCompactProgressWriter_AppliesTransformToCardPayloadEntries(t *testing.T) {
	p := &stubCompactProgressPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "feishu"},
		style:              "card",
		supportPayload:     true,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "codex", LangEnglish, func(s string) string {
		return strings.ReplaceAll(s, "/root/code/demo/src/app.ts:42", "📄 `src/app.ts:42`")
	}, "", "")

	if ok := w.AppendStructured(ProgressCardEntry{
		Kind: ProgressEntryThinking,
		Text: "Inspect /root/code/demo/src/app.ts:42",
	}, "Inspect /root/code/demo/src/app.ts:42"); !ok {
		t.Fatal("AppendStructured() = false, want true")
	}

	starts := p.getPreviewStarts()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want 1", len(starts))
	}
	payload, ok := ParseProgressCardPayload(starts[0])
	if !ok {
		t.Fatalf("ParseProgressCardPayload(%q) failed", starts[0])
	}
	if len(payload.Items) != 1 {
		t.Fatalf("payload items = %d, want 1", len(payload.Items))
	}
	if got := payload.Items[0].Text; got != "Inspect 📄 `src/app.ts:42`" {
		t.Fatalf("payload item text = %q, want transformed text", got)
	}
}

func TestCompactProgressWriter_DoesNotTransformToolResults(t *testing.T) {
	p := &stubCompactProgressPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "feishu"},
		style:              "card",
		supportPayload:     true,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "codex", LangEnglish, func(s string) string {
		return strings.ReplaceAll(s, "/root/code/demo/src/app.ts:42", "📄 `src/app.ts:42`")
	}, "", "")

	raw := "/root/code/demo/src/app.ts:42"
	if ok := w.AppendStructured(ProgressCardEntry{
		Kind: ProgressEntryToolResult,
		Text: raw,
	}, raw); !ok {
		t.Fatal("AppendStructured() = false, want true")
	}

	starts := p.getPreviewStarts()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want 1", len(starts))
	}
	payload, ok := ParseProgressCardPayload(starts[0])
	if !ok {
		t.Fatalf("ParseProgressCardPayload(%q) failed", starts[0])
	}
	if got := payload.Items[0].Text; got != raw {
		t.Fatalf("tool result text = %q, want raw %q", got, raw)
	}
}

// fakeRetryableErr mimics slack-go's *RateLimitedError shape: it carries a
// Retryable() bool method so the writer's classifier can treat it as transient.
type fakeRetryableErr struct{ msg string }

func (e *fakeRetryableErr) Error() string   { return e.msg }
func (e *fakeRetryableErr) Retryable() bool { return true }

func TestIsTransientUpdateErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("slack: update: %w", context.DeadlineExceeded), true},
		{"retryable error type", &fakeRetryableErr{msg: "rate limited"}, true},
		{"wrapped retryable", fmt.Errorf("slack: update: %w", &fakeRetryableErr{msg: "x"}), true},
		{"rate limit text", errors.New("slack: update: rate_limited"), true},
		{"429 text", errors.New("HTTP status 429"), true},
		{"503 text", errors.New("HTTP status 503"), true},
		{"plain auth error", errors.New("invalid_auth"), false},
		{"context canceled", context.Canceled, false},
		{"message not found", errors.New("message_not_found"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientUpdateErr(tc.err); got != tc.want {
				t.Fatalf("isTransientUpdateErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// progressTransientErrPlatform is a Platform stub whose UpdateMessage returns
// a scripted sequence of errors so we can exercise the writer's transient /
// fatal classification without pulling in Slack's real client.
type progressTransientErrPlatform struct {
	stubPlatformEngine
	mu      sync.Mutex
	errs    []error // returned in order; remainder beyond len is nil
	updates []string
	starts  int
}

func (p *progressTransientErrPlatform) ProgressStyle() string { return "compact" }

func (p *progressTransientErrPlatform) SendPreviewStart(_ context.Context, _ any, _ string) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	return "handle", nil
}

func (p *progressTransientErrPlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updates = append(p.updates, content)
	idx := len(p.updates) - 1
	if idx < len(p.errs) {
		return p.errs[idx]
	}
	return nil
}

func (p *progressTransientErrPlatform) updateCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.updates)
}

func TestCompactProgressWriter_TransientErrorDoesNotDisable(t *testing.T) {
	p := &progressTransientErrPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "slack"},
		// The first AppendStructured call creates the preview handle via
		// SendPreviewStart and does NOT hit UpdateMessage. Subsequent
		// appends are the ones that exercise UpdateMessage, so errs[0] is
		// the error returned for the 2nd append, errs[1] for the 3rd, etc.
		errs: []error{context.DeadlineExceeded, nil},
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	// 1st append: creates preview handle via SendPreviewStart; no UpdateMessage yet.
	if ok := w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "ls"}, "tool ls"); !ok {
		t.Fatal("first AppendStructured should succeed")
	}
	w.mu.Lock()
	w.stopTickerLocked()
	w.mu.Unlock()
	if p.starts != 1 {
		t.Fatalf("preview starts = %d, want 1", p.starts)
	}

	// 2nd append: fires UpdateMessage and returns context.DeadlineExceeded.
	// The writer must treat this as transient: NOT disable itself.
	if ok := w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "pwd"}, "tool pwd"); !ok {
		t.Fatal("AppendStructured after transient error should still report ok")
	}
	w.mu.Lock()
	if w.failed {
		t.Fatal("writer should NOT be marked failed after transient UpdateMessage error")
	}
	if !w.inCooldown() {
		t.Fatal("writer should be in cooldown after transient error")
	}
	// Simulate cooldown expiry so the next update is allowed through.
	w.cooldownUntil = time.Time{}
	w.stopTickerLocked()
	w.mu.Unlock()

	// 3rd append: now succeeds — writer has recovered.
	if ok := w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "whoami"}, "tool whoami"); !ok {
		t.Fatal("third AppendStructured should succeed after cooldown")
	}
	w.mu.Lock()
	w.stopTickerLocked()
	if w.failed {
		t.Fatal("writer should still not be failed after recovery")
	}
	w.mu.Unlock()

	if got := p.updateCount(); got != 2 {
		t.Fatalf("UpdateMessage calls = %d, want 2 (2nd + 3rd appends)", got)
	}
}

func TestCompactProgressWriter_PermanentErrorDisables(t *testing.T) {
	p := &progressTransientErrPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "slack"},
		errs:               []error{errors.New("invalid_auth")},
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	// Bootstrap: create the preview handle.
	_ = w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "ls"}, "tool ls")
	w.mu.Lock()
	w.stopTickerLocked()
	w.mu.Unlock()

	// This append triggers UpdateMessage -> invalid_auth (permanent).
	ok := w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "pwd"}, "tool pwd")
	if ok {
		t.Fatal("AppendStructured should return false on permanent error")
	}
	w.mu.Lock()
	if !w.failed {
		t.Fatal("writer should be marked failed after permanent UpdateMessage error")
	}
	w.stopTickerLocked()
	w.mu.Unlock()
}

func TestCompactProgressWriter_CooldownSkipsUpdate(t *testing.T) {
	p := &progressTransientErrPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "slack"},
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	// Bootstrap: create handle.
	_ = w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "ls"}, "tool ls")
	w.mu.Lock()
	w.cooldownUntil = time.Now().Add(5 * time.Second)
	w.stopTickerLocked()
	w.mu.Unlock()

	// While in cooldown, a further append must NOT hit the platform.
	_ = w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "pwd"}, "tool pwd")
	w.mu.Lock()
	w.stopTickerLocked()
	w.mu.Unlock()

	if got := p.updateCount(); got != 0 {
		t.Fatalf("UpdateMessage calls during cooldown = %d, want 0", got)
	}
}
