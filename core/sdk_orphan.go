package core

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// OrphanEventEmitter is implemented by AgentSessions whose underlying agent
// surfaces events arriving outside an active Send() turn — Claude Code's
// in-process ScheduleWakeup self-fires being the canonical case. The engine
// starts one orphan handler goroutine per session that drains this channel
// and renders each idle turn to the user's messaging platform with a wakeup
// header, so the user sees what the agent did instead of the response getting
// silently dropped on the floor.
//
// Lifecycle: the orphan handler runs until the OrphanEvents channel closes
// (which the agent does on session termination, alongside its primary Events
// channel). No explicit Stop is required.
type OrphanEventEmitter interface {
	OrphanEvents() <-chan Event
}

// orphanWaitForLockMax bounds how long the orphan handler will wait for the
// cc-connect session lock before giving up on rendering the wakeup turn. We
// retry on a short interval so a brief user-initiated turn doesn't drop the
// orphan output, but we also don't block forever if the user is mid-flow.
const orphanWaitForLockMax = 30 * time.Second

// orphanLockRetryInterval controls how often the handler probes the session
// lock while waiting for the active user turn to release it.
const orphanLockRetryInterval = 250 * time.Millisecond

// startOrphanHandlerForSession launches the orphan handler goroutine if the
// session implements OrphanEventEmitter. Idempotent in the sense that the
// caller is responsible for invoking it exactly once per session lifetime —
// i.e. when interactiveState is first created. The goroutine exits cleanly
// when the OrphanEvents channel closes (session ended).
func (e *Engine) startOrphanHandlerForSession(state *interactiveState) {
	if state == nil || state.agentSession == nil {
		return
	}
	emitter, ok := state.agentSession.(OrphanEventEmitter)
	if !ok {
		return
	}
	ch := emitter.OrphanEvents()
	if ch == nil {
		return
	}
	go e.orphanLoop(state, ch)
}

// orphanLoop reads events from the orphan channel and groups them into
// "turns" delimited by EventResult / terminal EventError / channel close.
// For each turn it renders the agent's text response to the platform with a
// wakeup header. Permission requests are auto-responded inline so the
// subprocess does not deadlock waiting on stdio.
func (e *Engine) orphanLoop(state *interactiveState, ch <-chan Event) {
	turn := newOrphanTurn()
	for evt := range ch {
		switch evt.Type {
		case EventPermissionRequest:
			e.respondOrphanPermission(state, evt)
		case EventThinking:
			// Don't surface raw thinking content in orphan turns; it can be
			// long and the cron-style header reads cleaner without it.
			_ = evt
		case EventToolUse:
			turn.toolCount++
			if evt.ToolName != "" {
				turn.toolNames = append(turn.toolNames, evt.ToolName)
			}
		case EventText:
			if evt.Content != "" {
				turn.text = append(turn.text, evt.Content)
			}
		case EventResult:
			e.flushOrphanTurn(state, turn, nil)
			turn = newOrphanTurn()
		case EventError:
			e.flushOrphanTurn(state, turn, evt.Error)
			turn = newOrphanTurn()
		}
	}
	// Channel closed (session ended). Flush any pending turn so partial
	// output isn't silently dropped on subprocess exit.
	if !turn.empty() {
		e.flushOrphanTurn(state, turn, nil)
	}
}

type orphanTurn struct {
	text      []string
	toolCount int
	toolNames []string
	startedAt time.Time
}

func newOrphanTurn() *orphanTurn {
	return &orphanTurn{startedAt: time.Now()}
}

func (t *orphanTurn) empty() bool {
	return len(t.text) == 0 && t.toolCount == 0
}

// respondOrphanPermission answers a permission request from an orphan turn
// inline so the Claude Code subprocess does not block waiting for a response.
// We auto-allow when the engine's underlying session is in bypassPermissions
// mode (the model was running unattended anyway), otherwise deny — the user
// is not present to approve a wakeup-initiated tool use.
func (e *Engine) respondOrphanPermission(state *interactiveState, evt Event) {
	state.mu.Lock()
	sess := state.agentSession
	state.mu.Unlock()
	if sess == nil {
		return
	}

	autoAllow := false
	if mg, ok := sess.(interface{ GetMode() string }); ok {
		mode := strings.TrimSpace(mg.GetMode())
		if mode == "bypassPermissions" || mode == "auto" {
			autoAllow = true
		}
	}

	res := PermissionResult{Behavior: "deny", Message: "Wakeup turn — user not present to approve."}
	if autoAllow {
		res = PermissionResult{Behavior: "allow", UpdatedInput: evt.ToolInputRaw}
	}
	if err := sess.RespondPermission(evt.RequestID, res); err != nil {
		slog.Warn("orphan: respond permission failed", "request_id", evt.RequestID, "tool", evt.ToolName, "error", err)
	}
}

// flushOrphanTurn renders one completed orphan turn to the platform. It waits
// briefly for the cc-connect session lock so a real user turn already in
// flight finishes first, then sends a single message with a wakeup header
// followed by the model's text response (and a brief tool-use summary when
// the model used tools). errOpt is non-nil when the turn ended in EventError.
func (e *Engine) flushOrphanTurn(state *interactiveState, turn *orphanTurn, errOpt error) {
	if state == nil || turn.empty() && errOpt == nil {
		return
	}

	state.mu.Lock()
	platform := state.platform
	replyCtx := state.replyCtx
	sessionKey := state.canonicalSessionKey
	state.mu.Unlock()
	if platform == nil {
		slog.Warn("orphan: no platform for session, dropping wakeup turn", "session_key", sessionKey, "text_parts", len(turn.text))
		return
	}

	// Reconstruct replyCtx if the state cached one is stale or missing —
	// the session may not have had a real user turn since startup.
	if replyCtx == nil {
		rc, err := reconstructOrphanReplyCtx(platform, sessionKey)
		if err != nil {
			slog.Warn("orphan: reconstruct replyCtx failed", "session_key", sessionKey, "error", err)
			return
		}
		replyCtx = rc
	}

	body := buildOrphanRender(turn, errOpt)
	if body == "" {
		return
	}

	// Wait for the cc-connect session lock so a concurrent user turn doesn't
	// interleave its messages with ours. If the user is busy beyond our
	// patience window, render anyway — better to surface the wakeup output
	// late than not at all.
	deadline := time.Now().Add(orphanWaitForLockMax)
	gotLock := false
	if sm := e.sessionManager(); sm != nil {
		if sess := sm.byKey(sessionKey); sess != nil {
			for time.Now().Before(deadline) {
				if sess.TryLock() {
					gotLock = true
					defer sess.Unlock()
					break
				}
				time.Sleep(orphanLockRetryInterval)
			}
		}
	}
	if !gotLock {
		slog.Info("orphan: rendering without session lock", "session_key", sessionKey, "wait", orphanWaitForLockMax)
	}

	e.send(platform, replyCtx, body)
}

// reconstructOrphanReplyCtx asks the platform to rebuild a reply context from
// the canonical session key. Mirrors the path cron and heartbeat use for
// proactive messaging.
func reconstructOrphanReplyCtx(platform Platform, sessionKey string) (any, error) {
	rcr, ok := platform.(ReplyContextReconstructor)
	if !ok {
		return nil, fmt.Errorf("platform %q does not support proactive messaging", platform.Name())
	}
	return rcr.ReconstructReplyCtx(sessionKey)
}

// sessionManager returns the engine's primary session manager, or nil when
// running in a stripped-down test harness that didn't wire one up. Used by
// the orphan handler to acquire the per-session lock.
func (e *Engine) sessionManager() *SessionManager {
	if e == nil {
		return nil
	}
	return e.sessions
}

// byKey resolves the cc-connect Session for a session key (e.g.
// "slack:C1:U1"), returning nil if no in-memory session exists yet. Read-only
// helper used by the orphan handler; the broader SessionManager API is
// activate/create-oriented and would side-effect here.
func (sm *SessionManager) byKey(key string) *Session {
	if sm == nil {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if id, ok := sm.activeSession[key]; ok {
		if s, ok := sm.sessions[id]; ok {
			return s
		}
	}
	return nil
}

// buildOrphanRender formats the wakeup header and the agent's response into a
// single message body suitable for a platform Send. Kept centralized so the
// header style is consistent and easy to tweak.
func buildOrphanRender(turn *orphanTurn, errOpt error) string {
	body := strings.TrimSpace(strings.Join(turn.text, ""))
	header := "💤 *Scheduled wakeup*"
	if turn.toolCount > 0 {
		header += fmt.Sprintf(" — used %d tool%s", turn.toolCount, plural(turn.toolCount))
	}
	if errOpt != nil {
		return header + "\n_failed: " + errOpt.Error() + "_"
	}
	if body == "" {
		// Tool-only turn with no text response; still let the user know the
		// model woke up so context divergence is visible.
		if turn.toolCount > 0 {
			return header + "\n_(no text response)_"
		}
		return ""
	}
	return header + "\n" + body
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
