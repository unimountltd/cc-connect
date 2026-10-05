package core

import (
	"context"
	"testing"
	"time"
)

// installLiveState registers a live, idle controllable session under key with
// the given last-activity time and returns both.
func installLiveState(e *Engine, key string, lastActivity time.Time) (*controllableAgentSession, *interactiveState) {
	sess := newControllableSession(key)
	state := &interactiveState{agentSession: sess, eventsNeedResync: false, lastActivity: lastActivity}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	return sess, state
}

func assertClosed(t *testing.T, sess *controllableAgentSession, name string) {
	t.Helper()
	select {
	case <-sess.closed:
	case <-time.After(time.Second):
		t.Fatalf("%s: expected session to be closed", name)
	}
}

func assertOpen(t *testing.T, sess *controllableAgentSession, name string) {
	t.Helper()
	select {
	case <-sess.closed:
		t.Fatalf("%s: session was closed but should have stayed live", name)
	default:
	}
}

func TestLiveSessionCap_EvictsLeastRecentlyUsedIdle(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(3))
	now := time.Now()
	oldest, _ := installLiveState(e, "test:a", now.Add(-3*time.Hour))
	middle, _ := installLiveState(e, "test:b", now.Add(-2*time.Hour))
	newest, _ := installLiveState(e, "test:c", now.Add(-1*time.Hour))

	if !e.reserveLiveSessionSlot(context.Background(), "test:d", nil, nil) {
		t.Fatal("expected a slot after evicting the LRU session")
	}
	assertClosed(t, oldest, "oldest")
	assertOpen(t, middle, "middle")
	assertOpen(t, newest, "newest")
	waitForInteractiveStateRemoved(t, e, "test:a")
	if n := e.liveSessionCount(); n != 2 {
		t.Fatalf("live count = %d, want 2 (room for the new one)", n)
	}
}

func TestLiveSessionCap_NoCapLeavesEverythingAlone(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(0))
	a, _ := installLiveState(e, "test:a", time.Now().Add(-time.Hour))
	b, _ := installLiveState(e, "test:b", time.Now())
	if !e.reserveLiveSessionSlot(context.Background(), "test:c", nil, nil) {
		t.Fatal("unlimited cap must always grant a slot")
	}
	assertOpen(t, a, "a")
	assertOpen(t, b, "b")
}

func TestLiveSessionCap_AlreadyLiveKeyNeedsNoSlot(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(1))
	a, _ := installLiveState(e, "test:a", time.Now())
	if !e.reserveLiveSessionSlot(context.Background(), "test:a", nil, nil) {
		t.Fatal("a key that is already live must not need a slot")
	}
	assertOpen(t, a, "a")
}

func TestLiveSessionCap_NeverEvictsBusySession(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(2))
	now := time.Now()
	midTurn, midTurnState := installLiveState(e, "test:turn", now.Add(-5*time.Hour))
	beginStateTurn(midTurnState)
	defer endStateTurn(midTurnState)
	permPending, permState := installLiveState(e, "test:perm", now.Add(-4*time.Hour))
	permState.mu.Lock()
	permState.pending = &pendingPermission{}
	permState.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if e.reserveLiveSessionSlot(ctx, "test:new", nil, nil) {
		t.Fatal("expected no slot while every live session is busy")
	}
	assertOpen(t, midTurn, "mid-turn")
	assertOpen(t, permPending, "permission pending")
}

func TestLiveSessionCap_QueuedMessagesBlockEviction(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(1))
	busy, busyState := installLiveState(e, "test:queued", time.Now().Add(-time.Hour))
	busyState.mu.Lock()
	busyState.pendingMessages = []queuedMessage{{}}
	busyState.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if e.reserveLiveSessionSlot(ctx, "test:new", nil, nil) {
		t.Fatal("expected no slot: the only live session has queued messages")
	}
	assertOpen(t, busy, "queued")
}

func TestLiveSessionCap_WaitsUntilBusySessionFrees(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(1))
	busy, busyState := installLiveState(e, "test:busy", time.Now())
	beginStateTurn(busyState)

	got := make(chan bool, 1)
	go func() { got <- e.reserveLiveSessionSlot(context.Background(), "test:new", nil, nil) }()

	// Let the waiter observe the busy state, then finish the turn.
	time.Sleep(20 * time.Millisecond)
	endStateTurn(busyState)

	select {
	case ok := <-got:
		if !ok {
			t.Fatal("expected the waiter to get a slot once the busy session finished")
		}
	case <-time.After(3 * liveSlotPollInterval):
		t.Fatal("waiter did not wake up after the busy session became idle")
	}
	assertClosed(t, busy, "busy-then-idle")
}

func TestLiveSessionCap_SharedAcrossEngines(t *testing.T) {
	limiter := NewLiveSessionLimiter(2)
	e1 := newTestEngine()
	e2 := newTestEngine()
	e1.SetLiveSessionLimiter(limiter)
	e2.SetLiveSessionLimiter(limiter)
	now := time.Now()
	older, _ := installLiveState(e1, "p1:a", now.Add(-2*time.Hour))
	newer, _ := installLiveState(e2, "p2:a", now.Add(-1*time.Hour))

	if !e2.reserveLiveSessionSlot(context.Background(), "p2:b", nil, nil) {
		t.Fatal("expected a slot via cross-engine eviction")
	}
	assertClosed(t, older, "older (other engine)")
	assertOpen(t, newer, "newer")
	if n := limiter.liveCount(); n != 1 {
		t.Fatalf("instance live count = %d, want 1", n)
	}
}

func TestLiveSessionCap_PerProjectCap(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(0)) // instance-wide unlimited
	e.SetMaxLiveAgentSessions(1)
	only, _ := installLiveState(e, "test:a", time.Now().Add(-time.Hour))
	if !e.reserveLiveSessionSlot(context.Background(), "test:b", nil, nil) {
		t.Fatal("expected a slot after per-project eviction")
	}
	assertClosed(t, only, "only")
}

func TestLiveSessionCap_EvictionKeepsSavedSessionIDForResume(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(1))
	sessions := e.sessionManager()
	sess := sessions.GetOrCreateActive("test:a")
	sess.SetAgentSessionID("claude-abc", "stub")
	live, state := installLiveState(e, "test:a", time.Now().Add(-time.Hour))
	state.mu.Lock()
	state.sessionManager = sessions
	state.canonicalSessionKey = "test:a"
	state.mu.Unlock()

	if !e.reserveLiveSessionSlot(context.Background(), "test:b", nil, nil) {
		t.Fatal("expected eviction to free a slot")
	}
	assertClosed(t, live, "evicted")
	if got := sess.GetAgentSessionID(); got != "claude-abc" {
		t.Fatalf("eviction must keep the agent session ID for --resume; got %q", got)
	}
}

func TestLiveSessionCap_LockedSessionCountsAsBusy(t *testing.T) {
	e := newTestEngine()
	e.SetLiveSessionLimiter(NewLiveSessionLimiter(1))
	sessions := e.sessionManager()
	sess := sessions.GetOrCreateActive("test:a")
	if !sess.TryLock() {
		t.Fatal("fresh session should lock")
	}
	defer sess.Unlock()
	live, state := installLiveState(e, "test:a", time.Now().Add(-time.Hour))
	state.mu.Lock()
	state.sessionManager = sessions
	state.canonicalSessionKey = "test:a"
	state.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if e.reserveLiveSessionSlot(ctx, "test:b", nil, nil) {
		t.Fatal("a session locked for a turn (e.g. wakeup/orphan turn) must not be evicted")
	}
	assertOpen(t, live, "locked")
}

func TestAgentSessionIdleTimeout_SkipsSessionMidTurn(t *testing.T) {
	e := newTestEngine()
	e.SetAgentSessionIdleTimeout(20 * time.Millisecond)
	key := "test:user1"
	sess := newControllableSession("s1")
	state := &interactiveState{agentSession: sess, eventsNeedResync: false}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	e.scheduleAgentSessionIdleClose(key, state)
	// A turn starts without cancelling the timer (simulates the race the
	// token guard exists for); the busy check must still protect it.
	beginStateTurn(state)
	defer endStateTurn(state)

	select {
	case <-sess.closed:
		t.Fatal("idle close took down a session with a turn in flight")
	case <-time.After(80 * time.Millisecond):
	}
}
