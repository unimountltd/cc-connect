package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestProcessInteractiveEvents_QuotaErrorIsFriendly verifies a quota error
// reaches the user as the friendly quota notice, never the raw CLI text
// (which names the vendor and links its billing page), and that queued
// senders get the same notice.
func TestProcessInteractiveEvents_QuotaErrorIsFriendly(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	agentSession.alive = false
	state := &interactiveState{
		agentSession:    agentSession,
		platform:        p,
		replyCtx:        "ctx-1",
		pendingMessages: []queuedMessage{{platform: p, replyCtx: "ctx-2", content: "queued"}},
	}
	e.interactiveMu.Lock()
	e.interactiveStates[sessionKey] = state
	e.interactiveMu.Unlock()

	raw := "You've reached your Fable limit. Switch to another model, or manage usage credits at claude.ai/settings/usage"
	agentSession.events <- Event{Type: EventError, Error: errors.New(raw), ErrorKind: ErrorKindQuota}

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, "ctx-1", telemetryMsgCtx{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processInteractiveEvents did not return")
	}

	want := e.i18n.T(MsgAgentQuotaExhausted)
	sent := p.getSent()
	if len(sent) != 2 || sent[0] != want || sent[1] != want {
		t.Fatalf("sent = %#v, want two quota notices", sent)
	}
	for _, s := range sent {
		if strings.Contains(s, "Fable") || strings.Contains(s, "claude") {
			t.Errorf("raw agent error leaked to user: %q", s)
		}
	}
}

func TestAgentErrorText(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	cases := []struct {
		kind ErrorKind
		err  error
		want MsgKey
	}{
		{ErrorKindQuota, errors.New("You've hit your weekly limit"), MsgAgentQuotaExhausted},
		{ErrorKindUnknown, errors.New("No conversation found: Session not found"), MsgSessionNotFound},
		{ErrorKindUnknown, errors.New("Claude Code process exited with code 1"), MsgAgentFailed},
		{ErrorKindUnknown, nil, MsgAgentFailed},
	}
	for _, tc := range cases {
		if got := e.agentErrorText(tc.kind, tc.err); got != e.i18n.T(tc.want) {
			t.Errorf("agentErrorText(%q, %v) = %q, want %q", tc.kind, tc.err, got, e.i18n.T(tc.want))
		}
	}
}
