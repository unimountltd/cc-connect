package core

import (
	"strings"
	"testing"
	"time"
)

// runStallTurn starts processInteractiveEvents for a fresh state and returns
// the platform, session and a channel closed when the event loop returns.
func runStallTurn(t *testing.T, stallAfter, idle time.Duration) (*stubPlatformEngine, *controllableAgentSession, <-chan struct{}) {
	t.Helper()
	p := &stubPlatformEngine{n: "test"}
	sess := newControllableSession("stall")
	e := NewEngine("test", &controllableAgent{nextSession: sess}, []Platform{p}, "", LangEnglish)
	e.SetStallNoticeTimeout(stallAfter)
	e.SetEventIdleTimeout(idle)

	key := "test:stall-user"
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	session.TryLock()

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "", time.Now(), nil, nil, nil, telemetryMsgCtx{})
		close(done)
	}()
	return p, sess, done
}

func countContaining(msgs []string, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// Regression: a wedged agent (no events after a tool result) left the chat
// silent for 56 minutes. The user must get one notice and the turn must stay
// alive so a late result is still delivered.
func TestStallNotice_FiresOnceAndKeepsTurnAlive(t *testing.T) {
	p, sess, done := runStallTurn(t, 50*time.Millisecond, 0)

	sess.events <- Event{Type: EventToolUse, ToolName: "Bash"}
	sess.events <- Event{Type: EventToolResult, ToolResult: "---"}

	// Several stall intervals pass with no events: exactly one notice.
	time.Sleep(250 * time.Millisecond)
	sent := p.getSent()
	if got := countContaining(sent, "No response from the agent"); got != 1 {
		t.Fatalf("stall notices = %d, want 1; sent=%v", got, sent)
	}
	if countContaining(sent, "still running") != 0 {
		t.Fatalf("tool already finished; notice must not claim it is running: %v", sent)
	}

	select {
	case <-done:
		t.Fatal("stall notice must not end the turn")
	default:
	}

	sess.events <- Event{Type: EventResult, Content: "late answer", Done: true}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not complete after late result")
	}
	if countContaining(p.getSent(), "late answer") != 1 {
		t.Fatalf("late result not delivered: %v", p.getSent())
	}
}

func TestStallNotice_NamesRunningToolAndRearmsAfterActivity(t *testing.T) {
	p, sess, done := runStallTurn(t, 50*time.Millisecond, 0)

	sess.events <- Event{Type: EventToolUse, ToolName: "Bash"}
	time.Sleep(120 * time.Millisecond)
	if got := countContaining(p.getSent(), "still running Bash"); got != 1 {
		t.Fatalf("want one running-tool notice, sent=%v", p.getSent())
	}

	// New activity re-arms the notice for the next quiet period.
	sess.events <- Event{Type: EventToolResult, ToolResult: "ok"}
	time.Sleep(120 * time.Millisecond)
	if got := countContaining(p.getSent(), "No response from the agent"); got != 1 {
		t.Fatalf("want notice re-armed after activity, sent=%v", p.getSent())
	}

	sess.events <- Event{Type: EventResult, Content: "done", Done: true}
	<-done
}

func TestStallNotice_NotSentWhileEventsFlow(t *testing.T) {
	p, sess, done := runStallTurn(t, 150*time.Millisecond, 0)

	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		sess.events <- Event{Type: EventText, Content: "working "}
	}
	sess.events <- Event{Type: EventResult, Content: "done", Done: true}
	<-done

	if got := countContaining(p.getSent(), "⏳"); got != 0 {
		t.Fatalf("no stall notice expected while events flow, sent=%v", p.getSent())
	}
}

func TestStallNotice_DisabledWhenZero(t *testing.T) {
	p, sess, done := runStallTurn(t, 0, 0)

	time.Sleep(100 * time.Millisecond)
	sess.events <- Event{Type: EventResult, Content: "done", Done: true}
	<-done

	if got := countContaining(p.getSent(), "⏳"); got != 0 {
		t.Fatalf("stall notice disabled, sent=%v", p.getSent())
	}
}

func TestStallNoticeText_RoundsToWholeMinutes(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	if got := e.stallNoticeText(10*time.Minute, ""); !strings.Contains(got, "10 min") {
		t.Fatalf("got %q", got)
	}
	if got := e.stallNoticeText(time.Second, "Bash"); !strings.Contains(got, "1 min") || !strings.Contains(got, "Bash") {
		t.Fatalf("got %q", got)
	}
}

// Regression: /stop discarded a queued message with "❌ Error: session reset",
// which neither said the message was dropped nor which message it was.
func TestStop_TellsQueuedSendersTheirMessageWasNotRun(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "test:user1"
	state := &interactiveState{
		agentSession: newControllableSession("stop-queued"),
		platform:     p,
		replyCtx:     "ctx-active",
		pendingMessages: []queuedMessage{
			{messageID: "q1", platform: p, replyCtx: "ctx-q1", content: "status\n  please"},
			{messageID: "q2", platform: p, replyCtx: "ctx-q2", content: ""},
		},
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	if !e.stopInteractiveSession(key, nil, nil) {
		t.Fatal("expected stop to find the session")
	}

	sent := p.getSent()
	if countContaining(sent, "“status please”") != 1 {
		t.Fatalf("want notice quoting the queued text, sent=%v", sent)
	}
	if countContaining(sent, "was not run") != 2 {
		t.Fatalf("want one notice per queued message, sent=%v", sent)
	}
	if countContaining(sent, "Error") != 0 {
		t.Fatalf("stop is not an error; sent=%v", sent)
	}
	state.mu.Lock()
	left := len(state.pendingMessages)
	state.mu.Unlock()
	if left != 0 {
		t.Fatalf("queue not drained: %d left", left)
	}
}
