package core

// Fork-only tests preserved across upstream merges. Kept in a separate file
// so upstream's engine_test.go can be replaced wholesale during merges
// without untangling fork test additions.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubProactiveMediaPlatform struct {
	stubMediaPlatform
	reconstructed  string
	reconstructErr error
}

func (p *stubProactiveMediaPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	if p.reconstructErr != nil {
		return nil, p.reconstructErr
	}
	p.reconstructed = sessionKey
	return "rebuilt-rctx:" + sessionKey, nil
}

func TestEngineSendToSessionWithAttachments_ProactiveAfterRestart(t *testing.T) {
	p := &stubProactiveMediaPlatform{stubMediaPlatform: stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}}
	store := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", &stubAgent{}, []Platform{p}, store, LangEnglish)

	// Simulate a persisted active session (as if cc-connect had been restarted
	// after the user previously interacted on Slack). No live interactiveState
	// exists in memory.
	sessionKey := "slack:C0ARKL97DJ5:U0ACLP3F532"
	e.sessions.GetOrCreateActive(sessionKey)

	if err := e.SendToSessionWithAttachments(
		"",
		"delivery ready",
		nil,
		[]FileAttachment{{MimeType: "text/plain", Data: []byte("doc"), FileName: "report.txt"}},
	); err != nil {
		t.Fatalf("SendToSessionWithAttachments returned error: %v", err)
	}
	if p.reconstructed != sessionKey {
		t.Fatalf("ReconstructReplyCtx called with %q, want %q", p.reconstructed, sessionKey)
	}
	if got := p.getSent(); len(got) != 1 || got[0] != "delivery ready" {
		t.Fatalf("sent text = %#v, want one message", got)
	}
	if len(p.files) != 1 || p.files[0].FileName != "report.txt" {
		t.Fatalf("files = %#v", p.files)
	}
}

func TestEngineSendToSessionWithAttachments_ProactiveExplicitKey(t *testing.T) {
	p := &stubProactiveMediaPlatform{stubMediaPlatform: stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "telegram"}}}
	store := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", &stubAgent{}, []Platform{p}, store, LangEnglish)

	// Two persisted active sessions on different platforms — caller must
	// disambiguate by passing an explicit session key.
	slackKey := "slack:C1:U1"
	tgKey := "telegram:1234:1234"
	e.sessions.GetOrCreateActive(slackKey)
	e.sessions.GetOrCreateActive(tgKey)

	if err := e.SendToSessionWithAttachments(tgKey, "hello", nil, nil); err != nil {
		t.Fatalf("SendToSessionWithAttachments returned error: %v", err)
	}
	if p.reconstructed != tgKey {
		t.Fatalf("ReconstructReplyCtx called with %q, want %q", p.reconstructed, tgKey)
	}
}

func TestEngineSendToSessionWithAttachments_ProactiveAmbiguous(t *testing.T) {
	p := &stubProactiveMediaPlatform{stubMediaPlatform: stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}}
	store := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", &stubAgent{}, []Platform{p}, store, LangEnglish)

	e.sessions.GetOrCreateActive("slack:C1:U1")
	e.sessions.GetOrCreateActive("slack:C2:U2")

	err := e.SendToSessionWithAttachments("", "hi", nil, nil)
	if err == nil {
		t.Fatal("expected ambiguous-session error")
	}
	if !strings.Contains(err.Error(), "specify --session") {
		t.Fatalf("err = %v, want hint about --session", err)
	}
}

func TestEngineSendToSessionWithAttachments_ProactiveUnsupported(t *testing.T) {
	// Plain stubMediaPlatform — does NOT implement ReplyContextReconstructor.
	p := &stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	store := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", &stubAgent{}, []Platform{p}, store, LangEnglish)

	e.sessions.GetOrCreateActive("slack:C1:U1")

	err := e.SendToSessionWithAttachments("", "hi", nil, nil)
	if err == nil {
		t.Fatal("expected unsupported-platform error")
	}
	if !strings.Contains(err.Error(), "does not support proactive sends") {
		t.Fatalf("err = %v, want unsupported-proactive hint", err)
	}
}

func TestEngineSendToSessionWithAttachments_ProactiveNoSessions(t *testing.T) {
	p := &stubProactiveMediaPlatform{stubMediaPlatform: stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}}
	store := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", &stubAgent{}, []Platform{p}, store, LangEnglish)

	err := e.SendToSessionWithAttachments("", "hi", nil, nil)
	if err == nil {
		t.Fatal("expected no-active-session error")
	}
	if !strings.Contains(err.Error(), "no active session found") {
		t.Fatalf("err = %v, want no-active-session message", err)
	}
}

func TestExecuteCardAction_ModelCleansUpWithInteractiveKey(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	agent := &stubModelModeAgent{model: "old"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	sessionKey := "feishu:channel1:user1"

	e.interactiveMu.Lock()
	e.interactiveStates[sessionKey] = &interactiveState{}
	e.interactiveMu.Unlock()

	e.executeCardAction("/model", "new-model", sessionKey)

	if agent.model != "new-model" {
		t.Errorf("model = %q, want new-model", agent.model)
	}

	e.interactiveMu.Lock()
	_, exists := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if exists {
		t.Error("expected interactive state to be cleaned up after /model")
	}
}

func TestExecuteCardAction_ModelUsesWorkspaceContext(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	globalAgent := &stubModelModeAgent{model: "global-old"}
	e := NewEngine("test", globalAgent, []Platform{p}, "", LangEnglish)

	baseDir := t.TempDir()
	bindingPath := filepath.Join(t.TempDir(), "bindings.json")
	e.SetMultiWorkspace(baseDir, bindingPath)

	wsDir := normalizeWorkspacePath(t.TempDir())
	channelID := "channel1"
	sessionKey := "feishu:" + channelID + ":user1"
	e.workspaceBindings.Bind("project:test", channelID, "chan", wsDir)

	ws := e.workspacePool.GetOrCreate(wsDir)
	wsAgent := &stubModelModeAgent{model: "workspace-old"}
	ws.agent = wsAgent
	ws.sessions = NewSessionManager("")

	interactiveKey := e.interactiveKeyForSessionKey(sessionKey)
	e.interactiveMu.Lock()
	e.interactiveStates[interactiveKey] = &interactiveState{}
	e.interactiveMu.Unlock()

	globalSession := e.sessions.GetOrCreateActive(sessionKey)
	globalSession.SetAgentSessionID("global-session", "test")
	wsSession := ws.sessions.GetOrCreateActive(sessionKey)
	wsSession.SetAgentSessionID("workspace-session", "test")

	e.executeCardAction("/model", "switch 1", sessionKey)

	if wsAgent.model != "gpt-4.1" {
		t.Fatalf("workspace agent model = %q, want gpt-4.1", wsAgent.model)
	}
	if globalAgent.model != "global-old" {
		t.Fatalf("global agent model = %q, want unchanged", globalAgent.model)
	}
	if got := ws.sessions.GetOrCreateActive(sessionKey).AgentSessionID; got != "" {
		t.Fatalf("workspace session id = %q, want cleared", got)
	}
	if got := e.sessions.GetOrCreateActive(sessionKey).AgentSessionID; got != "global-session" {
		t.Fatalf("global session id = %q, want untouched", got)
	}

	e.interactiveMu.Lock()
	_, exists := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()
	if exists {
		t.Error("expected workspace interactive state to be cleaned up after /model")
	}
}

func TestBareStop_ShortCircuitsToStopCommand(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "test:user1"

	state := &interactiveState{
		agentSession: newControllableSession("s1"),
		platform:     p,
		replyCtx:     "ctx",
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	for _, input := range []string{"stop", "Stop", "STOP", "sToP"} {
		// Re-add state for each iteration since /stop removes it
		e.interactiveMu.Lock()
		if _, ok := e.interactiveStates[key]; !ok {
			e.interactiveStates[key] = &interactiveState{
				agentSession: newControllableSession("s1"),
				platform:     p,
				replyCtx:     "ctx",
			}
		}
		e.interactiveMu.Unlock()

		msg := &Message{
			SessionKey: key,
			Platform:   "test",
			Content:    input,
			ReplyCtx:   "ctx",
		}
		e.handleMessage(p, msg)

		e.interactiveMu.Lock()
		_, exists := e.interactiveStates[key]
		e.interactiveMu.Unlock()
		if exists {
			t.Fatalf("input %q: expected interactive state to be removed", input)
		}
	}

	// Verify "stop" within a sentence does NOT trigger
	e.interactiveMu.Lock()
	e.interactiveStates[key] = &interactiveState{
		agentSession: newControllableSession("s2"),
		platform:     p,
		replyCtx:     "ctx",
	}
	e.interactiveMu.Unlock()

	msg := &Message{
		SessionKey: key,
		Platform:   "test",
		Content:    "stop the server",
		ReplyCtx:   "ctx",
	}
	e.handleMessage(p, msg)

	e.interactiveMu.Lock()
	_, stillExists := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if !stillExists {
		t.Fatal("\"stop the server\" should NOT trigger stop")
	}
}

func TestBareNewSession_ShortCircuitsToNewCommand(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "test:user1"
	session := e.sessions.GetOrCreateActive(key)
	session.AddHistory("user", "hello")

	for _, input := range []string{"new session", "New Session", "NEW SESSION"} {
		msg := &Message{
			SessionKey: key,
			Platform:   "test",
			Content:    input,
			ReplyCtx:   "ctx",
		}
		e.handleMessage(p, msg)

		sent := p.getSent()
		found := false
		for _, s := range sent {
			if strings.Contains(s, "✅") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("input %q: expected new session confirmation, got %v", input, sent)
		}
		p.clearSent()
	}

	// "new session please" should NOT trigger
	msg := &Message{
		SessionKey: key,
		Platform:   "test",
		Content:    "new session please",
		ReplyCtx:   "ctx",
	}
	e.handleMessage(p, msg)
	// Should go to agent, not create a session
}

func TestCompactProgress_ToolUseDefersStopHint(t *testing.T) {
	p := &stubCompactProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	ok := w.AppendEvent(ProgressEntryToolUse, "Reading file", "Read", "Reading file")
	if !ok {
		t.Fatal("AppendEvent should succeed for compact writer")
	}

	starts := p.getPreviewStarts()
	if len(starts) == 0 {
		t.Fatal("expected at least one preview start")
	}
	hint := `send "stop" to abort`
	// Stop hint is deferred for tool use — should NOT appear immediately.
	if strings.Contains(starts[0], hint) {
		t.Fatalf("compact progress should NOT contain stop hint on initial tool use, got %q", starts[0])
	}
}

func TestCompactProgress_NonToolIncludesStopHint(t *testing.T) {
	p := &stubCompactProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	// Use an info event (non-tool, non-thinking) which includes the stop hint immediately.
	ok := w.AppendEvent(ProgressEntryInfo, "some info", "", "some info")
	if !ok {
		t.Fatal("AppendEvent should succeed for compact writer")
	}

	starts := p.getPreviewStarts()
	if len(starts) == 0 {
		t.Fatal("expected at least one preview start")
	}
	hint := `send "stop" to abort`
	if !strings.Contains(starts[0], hint) {
		t.Fatalf("compact progress should contain stop hint for non-tool/thinking events, got %q", starts[0])
	}
}

func TestCompactProgress_ThinkingDefersStopHint(t *testing.T) {
	p := &stubCompactProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	ok := w.AppendEvent(ProgressEntryThinking, "Planning approach", "", "💭 Thinking…")
	if !ok {
		t.Fatal("AppendEvent should succeed for compact writer")
	}

	starts := p.getPreviewStarts()
	if len(starts) == 0 {
		t.Fatal("expected at least one preview start")
	}
	// Thinking events now defer the stop hint to the ticker (like tool events).
	hint := `send "stop" to abort`
	if strings.Contains(starts[0], hint) {
		t.Fatalf("thinking should defer stop hint to ticker, but got immediate hint in %q", starts[0])
	}
	// Should be wrapped in code block.
	if !strings.HasPrefix(starts[0], "```\n") {
		t.Fatalf("compact progress should be wrapped in code block, got %q", starts[0])
	}
}

func TestCardProgress_DoesNotIncludeStopHint(t *testing.T) {
	p := &stubCompactProgressPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "test"},
		style:              "card",
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	ok := w.AppendEvent(ProgressEntryToolUse, "Reading file", "Read", "Reading file")
	if !ok {
		t.Fatal("AppendEvent should succeed for card writer")
	}

	starts := p.getPreviewStarts()
	if len(starts) == 0 {
		t.Fatal("expected at least one preview start")
	}
	hint := `send "stop" to abort`
	if strings.Contains(starts[0], hint) {
		t.Fatalf("card progress should NOT contain stop hint, got %q", starts[0])
	}
}

func TestCompactProgress_FinalizeStripsHint(t *testing.T) {
	p := &stubCompactProgressPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "claudecode", LangEnglish, nil, "", "")

	// Use an info event which includes the stop hint immediately.
	w.AppendEvent(ProgressEntryInfo, "Processing", "", "Processing")

	hint := `send "stop" to abort`
	starts := p.getPreviewStarts()
	if len(starts) == 0 || !strings.Contains(starts[0], hint) {
		t.Fatalf("expected hint in progress, got %q", starts)
	}

	w.Finalize(ProgressCardStateCompleted)

	edits := p.getPreviewEdits()
	if len(edits) == 0 {
		t.Fatal("Finalize should produce an edit to strip the hint")
	}
	lastEdit := edits[len(edits)-1]
	if strings.Contains(lastEdit, hint) {
		t.Fatalf("finalized compact progress should not contain hint, got %q", lastEdit)
	}
	if lastEdit == "" {
		t.Fatal("finalized content should not be empty")
	}
	// Should be wrapped in code block.
	if !strings.HasPrefix(lastEdit, "```\n") {
		t.Fatalf("finalized compact progress should be wrapped in code block, got %q", lastEdit)
	}
}

func TestProcessInteractiveEvents_ReturnsRetriableKind(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	state := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     "ctx-1",
	}
	e.interactiveMu.Lock()
	e.interactiveStates[sessionKey] = state
	e.interactiveMu.Unlock()

	agentSession.events <- Event{
		Type:      EventError,
		Error:     errors.New(`{"error":{"type":"rate_limit_error"}}`),
		ErrorKind: ErrorKindRateLimit,
	}

	var got ErrorKind
	done := make(chan struct{})
	go func() {
		got = e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, "ctx-1", telemetryMsgCtx{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processInteractiveEvents did not return after EventError")
	}

	if got != ErrorKindRateLimit {
		t.Errorf("returned kind = %q, want %q", got, ErrorKindRateLimit)
	}

	// Must NOT have surfaced the error as a user-visible message — the caller
	// (processInteractiveMessageWith) decides whether to retry or show "gave up".
	for _, s := range p.getSent() {
		if strings.Contains(s, "Error:") || strings.Contains(s, "错误") {
			t.Errorf("event loop surfaced error during retriable failure: %q", s)
		}
	}
}

func TestProcessInteractiveEvents_ReturnsUnknownOnNonRetriable(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	agentSession.alive = false // non-retriable errors usually mean the session is dead
	state := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     "ctx-1",
	}
	e.interactiveMu.Lock()
	e.interactiveStates[sessionKey] = state
	e.interactiveMu.Unlock()

	agentSession.events <- Event{
		Type:      EventError,
		Error:     errors.New("compilation failed"),
		ErrorKind: ErrorKindUnknown,
	}

	var got ErrorKind
	done := make(chan struct{})
	go func() {
		got = e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, "ctx-1", telemetryMsgCtx{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processInteractiveEvents did not return")
	}

	if got != ErrorKindUnknown {
		t.Errorf("returned kind = %q, want empty", got)
	}
	sent := p.getSent()
	sawError := false
	for _, s := range sent {
		if strings.Contains(s, "Error") || strings.Contains(s, "错误") {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("non-retriable error not surfaced to platform; sent = %#v", sent)
	}
}

func TestProcessInteractiveEvents_ReturnsUnknownOnSuccess(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	sessionKey := "test:user1"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("s1")
	state := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     "ctx-1",
	}
	e.interactiveMu.Lock()
	e.interactiveStates[sessionKey] = state
	e.interactiveMu.Unlock()

	agentSession.events <- Event{Type: EventResult, Content: "done", Done: true}

	var got ErrorKind
	done := make(chan struct{})
	go func() {
		got = e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m1", time.Now(), nil, nil, "ctx-1", telemetryMsgCtx{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processInteractiveEvents did not return")
	}

	if got != ErrorKindUnknown {
		t.Errorf("returned kind = %q, want empty", got)
	}
}

func TestRetryLoop_RetriesOnRateLimitThenSucceeds(t *testing.T) {
	withShortRateLimitDelays(t, 10*time.Millisecond, 10*time.Millisecond, 30)

	p := &stubPlatformEngine{n: "test"}

	// First session emits a retriable error. Second session emits success.
	sess1 := newQueuingSession("s1")
	sess2 := newQueuingSession("s2")
	agent := &scriptedAgent{sessions: []*queuingAgentSession{sess1, sess2}}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	sessionKey := "test:user1"

	// Inject each session's events after we confirm Send was called.
	// drainEvents runs before Send in processInteractiveMessageWith, so
	// pre-queued events would be drained away.
	go func() {
		waitForSend(t, sess1, 3*time.Second)
		sess1.events <- Event{
			Type:      EventError,
			Error:     errors.New(`{"error":{"type":"rate_limit_error"}}`),
			ErrorKind: ErrorKindRateLimit,
		}
		waitForSend(t, sess2, 3*time.Second)
		sess2.events <- Event{Type: EventResult, Content: "hello world", Done: true}
	}()

	msg := &Message{
		SessionKey: sessionKey,
		Platform:   "test",
		UserID:     "u1",
		UserName:   "user",
		Content:    "what's up",
		ReplyCtx:   "ctx",
	}
	e.handleMessage(p, msg)

	// Wait for success reply.
	deadline := time.After(3 * time.Second)
	for {
		sent := p.getSent()
		sawRetry := false
		sawSuccess := false
		for _, s := range sent {
			if strings.Contains(s, "Retry 1/") || strings.Contains(s, "rate limited") {
				sawRetry = true
			}
			if strings.Contains(s, "hello world") {
				sawSuccess = true
			}
		}
		if sawRetry && sawSuccess {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for retry + success; sent = %#v", sent)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := agent.StartCount(); got != 2 {
		t.Errorf("agent.StartSession calls = %d, want 2 (original + retry)", got)
	}
	// Wait for the background goroutine to release the session lock before
	// the test's deferred cleanup touches the global delays.
	waitForSessionIdle(t, e.sessions.GetOrCreateActive(sessionKey), 2*time.Second)
}

func TestRetryLoop_GivesUpAtCap(t *testing.T) {
	withShortRateLimitDelays(t, 5*time.Millisecond, 5*time.Millisecond, 3) // 3 total attempts

	p := &stubPlatformEngine{n: "test"}

	// Three sessions all return rate-limit errors.
	sess1 := newQueuingSession("s1")
	sess2 := newQueuingSession("s2")
	sess3 := newQueuingSession("s3")
	agent := &scriptedAgent{sessions: []*queuingAgentSession{sess1, sess2, sess3}}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	sessionKey := "test:user1"

	go func() {
		rateLimitEvt := Event{
			Type:      EventError,
			Error:     errors.New(`{"error":{"type":"rate_limit_error"}}`),
			ErrorKind: ErrorKindRateLimit,
		}
		for _, s := range []*queuingAgentSession{sess1, sess2, sess3} {
			waitForSend(t, s, 3*time.Second)
			s.events <- rateLimitEvt
		}
	}()

	msg := &Message{
		SessionKey: sessionKey,
		Platform:   "test",
		UserID:     "u1",
		UserName:   "user",
		Content:    "what's up",
		ReplyCtx:   "ctx",
	}
	e.handleMessage(p, msg)

	deadline := time.After(3 * time.Second)
	for {
		sent := p.getSent()
		for _, s := range sent {
			if strings.Contains(s, "did not clear") || strings.Contains(s, "gave up") || strings.Contains(s, "未解除") {
				goto done
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 'gave up' message; sent = %#v", sent)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
done:
	if got := agent.StartCount(); got != 3 {
		t.Errorf("agent.StartSession calls = %d, want 3 (all attempts)", got)
	}
	waitForSessionIdle(t, e.sessions.GetOrCreateActive(sessionKey), 2*time.Second)
}

func TestRetryLoop_StopCancelsWait(t *testing.T) {
	// Long delay so the test has time to send the stop signal before the
	// retry fires.
	withShortRateLimitDelays(t, 5*time.Second, 5*time.Second, 30)

	p := &stubPlatformEngine{n: "test"}
	sess1 := newQueuingSession("s1")
	sess2 := newQueuingSession("s2") // won't actually be used
	agent := &scriptedAgent{sessions: []*queuingAgentSession{sess1, sess2}}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	sessionKey := "test:user1"

	go func() {
		waitForSend(t, sess1, 3*time.Second)
		sess1.events <- Event{
			Type:      EventError,
			Error:     errors.New(`{"error":{"type":"rate_limit_error"}}`),
			ErrorKind: ErrorKindRateLimit,
		}
	}()

	msg := &Message{
		SessionKey: sessionKey,
		Platform:   "test",
		UserID:     "u1",
		UserName:   "user",
		Content:    "what's up",
		ReplyCtx:   "ctx",
	}

	handleDone := make(chan struct{})
	go func() {
		e.handleMessage(p, msg)
		close(handleDone)
	}()

	// Wait until the retry notification has been sent so we know we're in the
	// post-first-attempt wait.
	deadline := time.After(2 * time.Second)
	for {
		sawRetry := false
		for _, s := range p.getSent() {
			if strings.Contains(s, "Retry 1/") {
				sawRetry = true
				break
			}
		}
		if sawRetry {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for retry notification to arrive")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Now send stop signal.
	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state == nil {
		t.Fatal("interactive state missing")
	}
	state.markStopped()

	// handleMessage should return promptly (not wait out the 5s delay).
	select {
	case <-handleDone:
	case <-time.After(1 * time.Second):
		t.Fatal("handleMessage did not return after stop signal")
	}

	// Wait for the background goroutine to release the session lock before
	// cleanup so the race detector is happy.
	waitForSessionIdle(t, e.sessions.GetOrCreateActive(sessionKey), 2*time.Second)

	// Agent should have been started once (the second session is untouched).
	if got := agent.StartCount(); got != 1 {
		t.Errorf("agent.StartSession calls = %d, want 1 (stop before retry)", got)
	}
}


// ── helpers ──────────────────────────────────────────

func withShortRateLimitDelays(t *testing.T, initial, retry time.Duration, maxAttempts int) {
	t.Helper()
	oldInitial, oldRetry, oldMax := RateLimitInitialDelay, RateLimitRetryDelay, RateLimitMaxAttempts
	RateLimitInitialDelay = initial
	RateLimitRetryDelay = retry
	RateLimitMaxAttempts = maxAttempts
	t.Cleanup(func() {
		RateLimitInitialDelay = oldInitial
		RateLimitRetryDelay = oldRetry
		RateLimitMaxAttempts = oldMax
	})
}

type scriptedAgent struct {
	mu       sync.Mutex
	sessions []*queuingAgentSession
	starts   int
}

func waitForSend(t *testing.T, sess *queuingAgentSession, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		sess.sendMu.Lock()
		n := len(sess.sendCalls)
		sess.sendMu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Send to be called")
}

func waitForSessionIdle(t *testing.T, session *Session, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if session.TryLock() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for session to become idle")
}

func (a *scriptedAgent) Name() string { return "scripted" }

func (a *scriptedAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts++
	if len(a.sessions) == 0 {
		return nil, fmt.Errorf("scriptedAgent: no more sessions")
	}
	s := a.sessions[0]
	a.sessions = a.sessions[1:]
	return s, nil
}

func (a *scriptedAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}

func (a *scriptedAgent) Stop() error { return nil }

func (a *scriptedAgent) StartCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts
}

