package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// orphanFakeSession satisfies AgentSession + OrphanEventEmitter and lets
// tests pump arbitrary event sequences through the orphan loop.
type orphanFakeSession struct {
	events       chan Event
	orphanEvents chan Event
	mode         string
	permResults  []PermissionResult
	mu           sync.Mutex
}

func newOrphanFakeSession() *orphanFakeSession {
	return &orphanFakeSession{
		events:       make(chan Event, 8),
		orphanEvents: make(chan Event, 32),
	}
}

func (s *orphanFakeSession) Send(prompt string, _ []ImageAttachment, _ []FileAttachment) error {
	return nil
}
func (s *orphanFakeSession) RespondPermission(_ string, res PermissionResult) error {
	s.mu.Lock()
	s.permResults = append(s.permResults, res)
	s.mu.Unlock()
	return nil
}
func (s *orphanFakeSession) Events() <-chan Event       { return s.events }
func (s *orphanFakeSession) OrphanEvents() <-chan Event { return s.orphanEvents }
func (s *orphanFakeSession) CurrentSessionID() string   { return "fake-session" }
func (s *orphanFakeSession) Alive() bool                { return true }
func (s *orphanFakeSession) Close() error               { return nil }
func (s *orphanFakeSession) GetMode() string            { return s.mode }

func (s *orphanFakeSession) recordedPermissions() []PermissionResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PermissionResult, len(s.permResults))
	copy(out, s.permResults)
	return out
}

// orphanReplyCtxPlatform satisfies Platform + ReplyContextReconstructor and
// records every Send call so tests can assert the rendered output.
type orphanReplyCtxPlatform struct {
	stubPlatformEngine
	rcKey string
}

func (p *orphanReplyCtxPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	p.rcKey = sessionKey
	return "rebuilt-rctx", nil
}

func newOrphanTestEngine(t *testing.T, plat Platform, sess AgentSession) (*Engine, *interactiveState) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := &Engine{
		ctx:      ctx,
		name:     "orphan-test",
		i18n:     NewI18n(LangEnglish),
		sessions: NewSessionManager(""),
	}
	state := &interactiveState{
		agentSession:        sess,
		platform:            plat,
		replyCtx:            "user-rctx",
		canonicalSessionKey: "slack:C1:U1",
	}
	return e, state
}

// TestOrphanLoop_RendersTextWithWakeupHeader verifies the happy path: a
// text-only orphan turn is delivered as a single platform message with the
// wakeup header and the model's response body.
func TestOrphanLoop_RendersTextWithWakeupHeader(t *testing.T) {
	plat := &orphanReplyCtxPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	sess := newOrphanFakeSession()
	e, state := newOrphanTestEngine(t, plat, sess)

	go e.orphanLoop(state, sess.orphanEvents)

	sess.orphanEvents <- Event{Type: EventText, Content: "wakeup ran the build, all green"}
	sess.orphanEvents <- Event{Type: EventResult, Done: true}
	close(sess.orphanEvents)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(plat.getSent()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := plat.getSent()
	if len(sent) == 0 {
		t.Fatal("no message sent to platform")
	}
	if !strings.Contains(sent[0], "Scheduled wakeup") {
		t.Errorf("expected wakeup header in output: %q", sent[0])
	}
	if !strings.Contains(sent[0], "wakeup ran the build, all green") {
		t.Errorf("expected response body in output: %q", sent[0])
	}
}

// TestOrphanLoop_PermissionRequestRespondedInline verifies that a permission
// request arriving on the orphan channel is responded to immediately so the
// subprocess does not deadlock waiting on stdio. Default mode (no auto-allow)
// denies; bypassPermissions auto-allows.
func TestOrphanLoop_PermissionRequestRespondedInline(t *testing.T) {
	t.Run("default mode denies", func(t *testing.T) {
		plat := &orphanReplyCtxPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
		sess := newOrphanFakeSession()
		e, state := newOrphanTestEngine(t, plat, sess)
		go e.orphanLoop(state, sess.orphanEvents)

		sess.orphanEvents <- Event{
			Type:      EventPermissionRequest,
			RequestID: "req-1",
			ToolName:  "Bash",
		}
		// Drain via terminal event so the loop exits cleanly.
		sess.orphanEvents <- Event{Type: EventResult, Done: true}
		close(sess.orphanEvents)

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(sess.recordedPermissions()) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		perms := sess.recordedPermissions()
		if len(perms) != 1 {
			t.Fatalf("want 1 permission response, got %d", len(perms))
		}
		if perms[0].Behavior != "deny" {
			t.Errorf("default mode should deny, got %q", perms[0].Behavior)
		}
	})

	t.Run("bypassPermissions auto-allows", func(t *testing.T) {
		plat := &orphanReplyCtxPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
		sess := newOrphanFakeSession()
		sess.mode = "bypassPermissions"
		e, state := newOrphanTestEngine(t, plat, sess)
		go e.orphanLoop(state, sess.orphanEvents)

		sess.orphanEvents <- Event{
			Type:         EventPermissionRequest,
			RequestID:    "req-2",
			ToolName:     "Bash",
			ToolInputRaw: map[string]any{"command": "ls"},
		}
		sess.orphanEvents <- Event{Type: EventResult, Done: true}
		close(sess.orphanEvents)

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if len(sess.recordedPermissions()) > 0 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		perms := sess.recordedPermissions()
		if len(perms) != 1 || perms[0].Behavior != "allow" {
			t.Fatalf("bypassPermissions should auto-allow, got %+v", perms)
		}
	})
}

// TestOrphanLoop_ToolUseSummary verifies the header notes the tool count
// when the model used tools but produced no text response.
func TestOrphanLoop_ToolUseSummary(t *testing.T) {
	plat := &orphanReplyCtxPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	sess := newOrphanFakeSession()
	e, state := newOrphanTestEngine(t, plat, sess)
	go e.orphanLoop(state, sess.orphanEvents)

	sess.orphanEvents <- Event{Type: EventToolUse, ToolName: "Bash"}
	sess.orphanEvents <- Event{Type: EventToolUse, ToolName: "Read"}
	sess.orphanEvents <- Event{Type: EventResult, Done: true}
	close(sess.orphanEvents)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(plat.getSent()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := plat.getSent()
	if len(sent) == 0 {
		t.Fatal("tool-only orphan turn produced no output (expected a no-text notice)")
	}
	if !strings.Contains(sent[0], "used 2 tools") {
		t.Errorf("expected tool count in header: %q", sent[0])
	}
}

// TestOrphanLoop_EmptyTurnSkipped verifies that a fully-empty orphan turn
// (no text, no tools, just EventResult) does not spam the channel with an
// empty wakeup message.
func TestOrphanLoop_EmptyTurnSkipped(t *testing.T) {
	plat := &orphanReplyCtxPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	sess := newOrphanFakeSession()
	e, state := newOrphanTestEngine(t, plat, sess)
	go e.orphanLoop(state, sess.orphanEvents)

	sess.orphanEvents <- Event{Type: EventResult, Done: true}
	close(sess.orphanEvents)

	time.Sleep(200 * time.Millisecond)
	if got := plat.getSent(); len(got) != 0 {
		t.Fatalf("empty orphan turn should not send anything, got %v", got)
	}
}

// TestOrphanLoop_ErrorTurnRendered verifies that a turn ending with
// EventError surfaces the failure to the user instead of dropping silently.
func TestOrphanLoop_ErrorTurnRendered(t *testing.T) {
	plat := &orphanReplyCtxPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	sess := newOrphanFakeSession()
	e, state := newOrphanTestEngine(t, plat, sess)
	go e.orphanLoop(state, sess.orphanEvents)

	sess.orphanEvents <- Event{Type: EventText, Content: "starting work..."}
	sess.orphanEvents <- Event{Type: EventError, Error: errors.New("rate limited")}
	close(sess.orphanEvents)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(plat.getSent()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sent := plat.getSent()
	if len(sent) == 0 {
		t.Fatal("error turn produced no output")
	}
	if !strings.Contains(sent[0], "rate limited") {
		t.Errorf("expected error message in output: %q", sent[0])
	}
}

// TestStartOrphanHandlerForSession_NoOpForNonEmitter verifies that sessions
// not implementing OrphanEventEmitter (e.g. codex, gemini) are ignored
// silently — the hook in interactiveState creation must not break those.
func TestStartOrphanHandlerForSession_NoOpForNonEmitter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &Engine{ctx: ctx, sessions: NewSessionManager("")}
	state := &interactiveState{agentSession: &stubAgentSession{}}
	// Must not panic and must not start any goroutines we'd notice.
	e.startOrphanHandlerForSession(state)
}

// TestBuildOrphanRender_HeaderShape spot-checks the rendered header so the
// user-facing text doesn't drift silently. Pure function — no I/O.
func TestBuildOrphanRender_HeaderShape(t *testing.T) {
	cases := []struct {
		name     string
		turn     *orphanTurn
		err      error
		want     string
		wantNone bool
	}{
		{
			name: "text only",
			turn: &orphanTurn{text: []string{"all green"}},
			want: "💤 *Scheduled wakeup*\nall green",
		},
		{
			name: "with one tool",
			turn: &orphanTurn{text: []string{"done"}, toolCount: 1, toolNames: []string{"Bash"}},
			want: "💤 *Scheduled wakeup* — used 1 tool\ndone",
		},
		{
			name: "with multiple tools",
			turn: &orphanTurn{text: []string{"finished"}, toolCount: 3},
			want: "💤 *Scheduled wakeup* — used 3 tools\nfinished",
		},
		{
			name:     "empty",
			turn:     &orphanTurn{},
			wantNone: true,
		},
		{
			name: "error",
			turn: &orphanTurn{},
			err:  errors.New("boom"),
			want: "💤 *Scheduled wakeup*\n_failed: boom_",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOrphanRender(tc.turn, tc.err)
			if tc.wantNone {
				if got != "" {
					t.Fatalf("want empty render, got %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("buildOrphanRender mismatch:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
