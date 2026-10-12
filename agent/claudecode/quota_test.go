package claudecode

import (
	"context"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func newQuotaTestSession(t *testing.T) *claudeSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cs := &claudeSession{
		events: make(chan core.Event, 8),
		ctx:    ctx,
	}
	cs.sessionID.Store("s1")
	cs.alive.Store(true)
	return cs
}

func quotaResult(msg string) map[string]any {
	// Shape observed in production logs: subtype "success" with is_error.
	return map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": msg}
}

// TestHandleResultQuotaFromRateLimitEvent verifies a "rejected"
// rate_limit_event marks the following error result as ErrorKindQuota,
// even when the message text is unrecognised.
func TestHandleResultQuotaFromRateLimitEvent(t *testing.T) {
	cs := newQuotaTestSession(t)
	cs.handleReadLoopLine(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"seven_day"}}`)
	cs.handleResult(quotaResult("Usage exhausted"))
	if evt := <-cs.events; evt.ErrorKind != core.ErrorKindQuota {
		t.Fatalf("ErrorKind = %q, want %q", evt.ErrorKind, core.ErrorKindQuota)
	}

	// An "allowed" event clears the flag: later failures are not quota.
	cs.handleReadLoopLine(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`)
	cs.handleResult(quotaResult("compilation failed"))
	if evt := <-cs.events; evt.ErrorKind != core.ErrorKindUnknown {
		t.Fatalf("ErrorKind = %q, want unknown after allowed", evt.ErrorKind)
	}
}

// TestHandleResultQuotaFromMessage covers the text fallback using the CLI's
// real quota messages.
func TestHandleResultQuotaFromMessage(t *testing.T) {
	for _, msg := range []string{
		"You've hit your weekly limit · resets 2am (Europe/London)",
		"You've reached your Fable limit. Switch to another model, or manage usage credits at claude.ai/settings/usage?from=cc_cli_limit_message, to continue.",
	} {
		cs := newQuotaTestSession(t)
		cs.handleResult(quotaResult(msg))
		if evt := <-cs.events; evt.ErrorKind != core.ErrorKindQuota {
			t.Errorf("%q: ErrorKind = %q, want %q", msg, evt.ErrorKind, core.ErrorKindQuota)
		}
	}
}

func TestIsQuotaMessage_RejectsOrdinaryText(t *testing.T) {
	for _, msg := range []string{
		"rate limit in the handler is wrong",
		"I think you've hit your limit of patience",
		"",
	} {
		if isQuotaMessage(msg) {
			t.Errorf("isQuotaMessage(%q) = true, want false", msg)
		}
	}
}
