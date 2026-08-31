package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A resumed Claude Code session replays a synthetic turn (the
// "<task-notification> ... status stopped" message injected when the previous
// process died with background work registered). It terminates in under a
// second with an empty result and zero token usage while the user's real turn
// is still running. Consuming it as the turn's result used to surface
// "(empty response)" and strand the real answer as stale events.
func TestProcessInteractiveEvents_IgnoresPhantomResult(t *testing.T) {
	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	state := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     "ctx-1",
	}
	e.interactiveStates[sessionKey] = state

	// Phantom: empty content, no tools, no usage at all.
	agentSession.events <- Event{Type: EventResult, Done: true}
	// The user's real turn, with usage as any live turn reports.
	agentSession.events <- Event{
		Type:                 EventResult,
		Content:              "here is the real answer",
		Done:                 true,
		CacheReadInputTokens: 428803,
		OutputTokens:         730,
	}

	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, state.replyCtx, telemetryMsgCtx{})

	// The reply carries a context-usage indicator, so match on the prefix.
	got := p.getSent()
	if len(got) != 1 || !strings.HasPrefix(got[0], "here is the real answer") {
		t.Fatalf("sent = %#v, want only the real answer", got)
	}
	if strings.Contains(got[0], e.i18n.T(MsgEmptyResponse)) {
		t.Fatalf("sent = %#v, phantom result leaked to the user", got)
	}
}

// A phantom result must not stall the turn forever: if nothing follows it
// within the grace window it is accepted as the turn's result after all.
func TestProcessInteractiveEvents_PhantomResultFallsBackAfterGrace(t *testing.T) {
	orig := phantomResultGrace
	phantomResultGrace = 20 * time.Millisecond
	defer func() { phantomResultGrace = orig }()

	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	state := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     "ctx-1",
	}
	e.interactiveStates[sessionKey] = state

	agentSession.events <- Event{Type: EventResult, Done: true}

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, state.replyCtx, telemetryMsgCtx{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not complete after the phantom grace window expired")
	}

	got := p.getSent()
	if len(got) != 1 || got[0] != e.i18n.T(MsgEmptyResponse) {
		t.Fatalf("sent = %#v, want %q", got, e.i18n.T(MsgEmptyResponse))
	}
}

// A result that is empty but reports usage is a real (silent) turn, not a
// phantom — it must be delivered immediately, without waiting out the grace.
func TestProcessInteractiveEvents_EmptyResultWithUsageIsNotPhantom(t *testing.T) {
	orig := phantomResultGrace
	phantomResultGrace = time.Hour // would hang the test if misclassified
	defer func() { phantomResultGrace = orig }()

	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	state := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     "ctx-1",
	}
	e.interactiveStates[sessionKey] = state

	agentSession.events <- Event{Type: EventResult, Done: true, CacheReadInputTokens: 12345}

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, state.replyCtx, telemetryMsgCtx{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("empty-but-real result was treated as a phantom")
	}

	if got := p.getSent(); len(got) != 1 || !strings.HasPrefix(got[0], e.i18n.T(MsgEmptyResponse)) {
		t.Fatalf("sent = %#v, want %q", got, e.i18n.T(MsgEmptyResponse))
	}
}

// fixedSessionAgent hands out a session the test controls, so HandleRelay can
// be driven event by event.
type fixedSessionAgent struct {
	stubAgent
	session AgentSession
}

func (a *fixedSessionAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return a.session, nil
}

// The relay path has the same failure mode: a phantom result would be relayed
// back to the calling bot as "(empty response)".
func TestHandleRelay_IgnoresPhantomResult(t *testing.T) {
	agentSession := newControllableSession("s1")
	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := NewEngine("test", &fixedSessionAgent{session: agentSession}, []Platform{p}, "", LangEnglish)

	agentSession.events <- Event{Type: EventResult, Done: true}
	agentSession.events <- Event{
		Type:                 EventResult,
		Content:              "relayed answer",
		Done:                 true,
		CacheReadInputTokens: 1000,
	}

	resp, err := e.HandleRelay(context.Background(), "other-bot", "test:chat-1:user", "ping")
	if err != nil {
		t.Fatalf("HandleRelay returned error: %v", err)
	}
	if resp != "relayed answer" {
		t.Fatalf("relay response = %q, want %q", resp, "relayed answer")
	}
}

// Text already streamed in this turn means the result belongs to a real turn
// even when the result payload itself is empty and unusaged.
func TestIsPhantomResult(t *testing.T) {
	empty := Event{Type: EventResult}
	if !isPhantomResult(empty, nil, 0) {
		t.Fatal("empty zero-usage result should be a phantom")
	}
	if isPhantomResult(empty, []string{"partial"}, 0) {
		t.Fatal("result with streamed text should not be a phantom")
	}
	if isPhantomResult(empty, nil, 3) {
		t.Fatal("result after tool calls should not be a phantom")
	}
	if isPhantomResult(Event{Type: EventResult, Content: "hi"}, nil, 0) {
		t.Fatal("result with content should not be a phantom")
	}
	for _, e := range []Event{
		{Type: EventResult, InputTokens: 1},
		{Type: EventResult, CacheCreationInputTokens: 1},
		{Type: EventResult, CacheReadInputTokens: 1},
		{Type: EventResult, OutputTokens: 1},
	} {
		if isPhantomResult(e, nil, 0) {
			t.Fatalf("result reporting usage should not be a phantom: %+v", e)
		}
	}
}
