package core

import (
	"strings"
	"testing"
	"time"
)

func sentContains(sent []string, sub string) bool {
	for _, s := range sent {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// runAutoCompressTurn drives one turn past a tiny auto-compress threshold and
// waits until /compact reaches the agent. It returns what the platform got.
func runAutoCompressTurn(t *testing.T, quiet bool) (*Engine, []string) {
	t.Helper()
	p := &stubPlatformEngine{n: "test"}
	sess := newQueuingSession("auto-compress-quiet")
	agent := &stubCompressorAgent{cmd: "/compact"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetAutoCompressConfig(true, 4, 0)
	e.SetQuietSessionNotices(quiet)

	key := "test:quiet"
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	session := e.sessions.GetOrCreateActive(key)
	session.AddHistory("user", "hello world")

	go e.processInteractiveEvents(state, session, e.sessions, key, "msg1", time.Now(), func() {}, nil, nil, telemetryMsgCtx{})
	sess.events <- Event{Type: EventResult, Content: "response", Done: true}

	deadline := time.After(2 * time.Second)
	for {
		sess.sendMu.Lock()
		n := len(sess.sendCalls)
		sess.sendMu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for auto-compress send")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Let the compress turn finish so completion messages (if any) are sent.
	sess.events <- Event{Type: EventResult, Content: "", Done: true}
	time.Sleep(50 * time.Millisecond)
	return e, p.getSent()
}

func TestAutoCompress_QuietSessionNoticesSuppressesNotice(t *testing.T) {
	e, sent := runAutoCompressTurn(t, true)
	if sentContains(sent, e.i18n.T(MsgCompressing)) {
		t.Fatalf("quiet_session_notices: compress notice must not be posted, sent=%q", sent)
	}
	if sentContains(sent, e.i18n.T(MsgCompressDone)) {
		t.Fatalf("quiet_session_notices: compress-done must not be posted, sent=%q", sent)
	}
	if !sentContains(sent, "response") {
		t.Fatalf("the turn's own answer must still be delivered, sent=%q", sent)
	}
}

func TestAutoCompress_DefaultStillPostsNotice(t *testing.T) {
	e, sent := runAutoCompressTurn(t, false)
	if !sentContains(sent, e.i18n.T(MsgCompressing)) {
		t.Fatalf("default: compress notice should be posted (upstream behaviour), sent=%q", sent)
	}
}

func idleResetFixture(t *testing.T, quiet bool, withLiveAgent bool) (*Engine, *stubPlatformEngine, *Session) {
	t.Helper()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetResetOnIdle(30 * time.Minute)
	e.SetQuietSessionNotices(quiet)

	sm := NewSessionManager(t.TempDir())
	session := sm.GetOrCreateActive("sk-quiet")
	session.AddHistory("user", "hello")
	session.SetAgentSessionID("agent-id-quiet", "claudecode")
	session.TryLock()
	session.mu.Lock()
	session.LastUserActivity = time.Now().Add(-45 * time.Minute)
	session.mu.Unlock()

	if withLiveAgent {
		e.interactiveMu.Lock()
		e.interactiveStates["ws:sk-quiet"] = &interactiveState{agentSession: newControllableSession("live"), platform: p, replyCtx: "ctx"}
		e.interactiveMu.Unlock()
	}

	msg := &Message{SessionKey: "sk-quiet", ReplyCtx: "ctx"}
	rotated := e.maybeAutoResetSessionOnIdle(p, msg, sm, "ws:sk-quiet", session)
	if rotated == nil {
		t.Fatal("expected idle reset to fire")
	}
	return e, p, rotated
}

func TestIdleReset_QuietSessionNoticesPostsNothing(t *testing.T) {
	e, p, _ := idleResetFixture(t, true, true)
	sent := p.getSent()
	if len(sent) != 0 {
		t.Fatalf("quiet_session_notices: idle reset must post nothing, sent=%q", sent)
	}
	_ = e
}

func TestIdleReset_DefaultPostsNotice(t *testing.T) {
	e, p, _ := idleResetFixture(t, false, false)
	want := e.i18n.Tf(MsgSessionAutoResetIdle, 30)
	if !sentContains(p.getSent(), want) {
		t.Fatalf("default: idle reset notice %q expected, sent=%q", want, p.getSent())
	}
}
