package core

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Live-session cap.
//
// Every interactive session key (one per Slack channel when
// share_session_in_channel is on) spawns its own agent process, and each
// process keeps its MCP servers alive for as long as it lives. Without a cap,
// an instance that has seen N channels holds N idle process trees forever.
//
// LiveSessionLimiter bounds the number of live agent processes across every
// Engine in this cc-connect instance. Before an engine spawns a new process it
// calls reserveLiveSessionSlot; when the cap is reached, the least recently
// active *idle* session anywhere in the instance is closed first (its saved
// agent session ID is kept, so the next message to that channel resumes). A
// session that is mid-turn, has a permission prompt open, or has queued
// messages is never evicted. If nothing is evictable, the caller waits for a
// slot instead of exceeding the cap.
type LiveSessionLimiter struct {
	max atomic.Int64

	mu      sync.Mutex
	engines []*Engine
}

// NewLiveSessionLimiter returns a limiter allowing at most max live agent
// processes across all engines. 0 means unlimited.
func NewLiveSessionLimiter(max int) *LiveSessionLimiter {
	l := &LiveSessionLimiter{}
	l.SetMax(max)
	return l
}

// SetMax updates the instance-wide cap. 0 disables the cap. Lowering the cap
// does not close sessions immediately; the next spawn evicts down to the cap.
func (l *LiveSessionLimiter) SetMax(max int) {
	if max < 0 {
		max = 0
	}
	l.max.Store(int64(max))
}

// Max returns the instance-wide cap (0 = unlimited).
func (l *LiveSessionLimiter) Max() int { return int(l.max.Load()) }

func (l *LiveSessionLimiter) attach(e *Engine) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.engines {
		if existing == e {
			return
		}
	}
	l.engines = append(l.engines, e)
}

func (l *LiveSessionLimiter) snapshotEngines() []*Engine {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Engine, len(l.engines))
	copy(out, l.engines)
	return out
}

// liveCount sums live agent processes across all attached engines.
func (l *LiveSessionLimiter) liveCount() int {
	n := 0
	for _, e := range l.snapshotEngines() {
		n += e.liveSessionCount()
	}
	return n
}

// evictLRU closes the least recently active idle session across all engines,
// skipping (exclude, excludeKey). Returns true once a session has been closed.
func (l *LiveSessionLimiter) evictLRU(exclude *Engine, excludeKey string) bool {
	var candidates []evictionCandidate
	for _, e := range l.snapshotEngines() {
		skip := ""
		if e == exclude {
			skip = excludeKey
		}
		candidates = append(candidates, e.idleEvictionCandidates(skip)...)
	}
	return evictOldest(candidates)
}

// evictionCandidate is a snapshot of a live, idle session taken under the
// engine locks. The token lets the close bail out if the session became busy
// between snapshot and close.
type evictionCandidate struct {
	engine       *Engine
	key          string
	state        *interactiveState
	token        uint64
	lastActivity time.Time
}

// evictOldest tries candidates from least to most recently active until one
// close succeeds.
func evictOldest(candidates []evictionCandidate) bool {
	for len(candidates) > 0 {
		oldest := 0
		for i := range candidates {
			if candidates[i].lastActivity.Before(candidates[oldest].lastActivity) {
				oldest = i
			}
		}
		c := candidates[oldest]
		if c.engine.evictSession(c) {
			return true
		}
		candidates = append(candidates[:oldest], candidates[oldest+1:]...)
	}
	return false
}

// liveSlotPollInterval is how often a spawn blocked on the cap re-checks for
// a free slot. liveSlotWaitMax bounds that wait so a message is not held
// forever when every other session is stuck mid-turn.
const (
	liveSlotPollInterval = 2 * time.Second
	liveSlotWaitMax      = 30 * time.Minute
)

// SetLiveSessionLimiter attaches the instance-wide limiter to this engine.
func (e *Engine) SetLiveSessionLimiter(l *LiveSessionLimiter) {
	e.liveLimiter = l
	if l != nil {
		l.attach(e)
	}
}

// SetMaxLiveAgentSessions caps live agent processes for this engine alone
// (per-project override). 0 = no per-project cap.
func (e *Engine) SetMaxLiveAgentSessions(n int) {
	if n < 0 {
		n = 0
	}
	e.maxLiveSessions.Store(int64(n))
}

// beginStateTurn / endStateTurn bracket foreground and unsolicited turns so
// eviction and idle close can tell a busy session from an idle one without
// relying on the idle-timer token.
func beginStateTurn(state *interactiveState) {
	if state == nil {
		return
	}
	state.mu.Lock()
	state.turnsInFlight++
	state.lastActivity = time.Now()
	state.mu.Unlock()
}

func endStateTurn(state *interactiveState) {
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.turnsInFlight > 0 {
		state.turnsInFlight--
	}
	state.lastActivity = time.Now()
	state.mu.Unlock()
}

// stateIsLiveLocked reports whether the state owns a live agent process.
// state.mu must be held.
func stateIsLiveLocked(state *interactiveState) bool {
	return state != nil && state.agentSession != nil && state.agentSession.Alive() && !state.stopped
}

// stateIsBusyLocked reports whether the session must not be closed right now:
// a turn is running, a permission prompt is open, messages are queued, the
// event stream needs a resync, or the cc-connect Session is locked for a turn
// (covers orphan/wakeup turns that bypass the engine's turn bookkeeping).
// state.mu must be held.
func (e *Engine) stateIsBusyLocked(state *interactiveState) bool {
	if state.turnsInFlight > 0 ||
		state.eventsNeedResync ||
		state.pending != nil ||
		len(state.pendingMessages) > 0 {
		return true
	}
	mgr := state.sessionManager
	if mgr == nil {
		mgr = e.sessionManager()
	}
	if mgr != nil && state.canonicalSessionKey != "" {
		if sess := mgr.byKey(state.canonicalSessionKey); sess != nil && sess.Busy() {
			return true
		}
	}
	return false
}

// liveSessionCount counts live agent processes owned by this engine.
func (e *Engine) liveSessionCount() int {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	n := 0
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		if stateIsLiveLocked(state) {
			n++
		}
		state.mu.Unlock()
	}
	return n
}

// isLiveSession reports whether sessionKey already owns a live process (in
// which case a message to it spawns nothing and needs no slot).
func (e *Engine) isLiveSession(sessionKey string) bool {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	state, ok := e.interactiveStates[sessionKey]
	if !ok {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return stateIsLiveLocked(state)
}

// idleEvictionCandidates snapshots every live, idle session except skipKey.
func (e *Engine) idleEvictionCandidates(skipKey string) []evictionCandidate {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	var out []evictionCandidate
	for key, state := range e.interactiveStates {
		if key == skipKey {
			continue
		}
		state.mu.Lock()
		if stateIsLiveLocked(state) && !e.stateIsBusyLocked(state) {
			out = append(out, evictionCandidate{
				engine:       e,
				key:          key,
				state:        state,
				token:        state.agentSessionIdleToken,
				lastActivity: state.lastActivity,
			})
		}
		state.mu.Unlock()
	}
	return out
}

// evictSession closes the candidate's live process, keeping the saved agent
// session ID so the next message resumes. Returns false if the session became
// busy or was replaced since the snapshot.
func (e *Engine) evictSession(c evictionCandidate) bool {
	return e.closeIdleInteractiveState(c.key, c.state, c.token, "evicted", 0)
}

// evictLocalLRU evicts the least recently active idle session in this engine.
func (e *Engine) evictLocalLRU(excludeKey string) bool {
	return evictOldest(e.idleEvictionCandidates(excludeKey))
}

// liveSlotAvailable reports whether spawning a process for sessionKey would
// stay within both the per-project and the instance-wide cap, evicting idle
// LRU sessions as needed. Returns false when the cap is reached and every
// other live session is busy.
func (e *Engine) liveSlotAvailable(sessionKey string) bool {
	for attempt := 0; attempt < 64; attempt++ {
		localMax := int(e.maxLiveSessions.Load())
		if localMax > 0 && e.liveSessionCount() >= localMax {
			if !e.evictLocalLRU(sessionKey) {
				return false
			}
			continue
		}
		if l := e.liveLimiter; l != nil {
			if globalMax := l.Max(); globalMax > 0 && l.liveCount() >= globalMax {
				if !l.evictLRU(e, sessionKey) {
					return false
				}
				continue
			}
		}
		return true
	}
	return false
}

// reserveLiveSessionSlot makes room for a new agent process for sessionKey.
// It returns immediately when the key is already live or no cap applies.
// Otherwise it evicts idle LRU sessions, and if every other session is busy it
// notifies the user once and waits (polling) until a slot frees up, the engine
// stops, or liveSlotWaitMax elapses. Returns false only when no slot could be
// obtained; the caller then reports the failure instead of spawning.
func (e *Engine) reserveLiveSessionSlot(ctx context.Context, sessionKey string, p Platform, replyCtx any) bool {
	localMax := int(e.maxLiveSessions.Load())
	globalMax := 0
	if e.liveLimiter != nil {
		globalMax = e.liveLimiter.Max()
	}
	if localMax <= 0 && globalMax <= 0 {
		return true
	}
	if e.isLiveSession(sessionKey) {
		return true
	}
	if e.liveSlotAvailable(sessionKey) {
		return true
	}

	slog.Info("live session cap reached and every session is busy, waiting for a slot",
		"session_key", sessionKey, "max_live", globalMax, "project_max_live", localMax)
	if p != nil {
		e.send(p, replyCtx, e.i18n.T(MsgSessionWaitingForSlot))
	}

	deadline := time.NewTimer(liveSlotWaitMax)
	defer deadline.Stop()
	ticker := time.NewTicker(liveSlotPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			slog.Warn("gave up waiting for a live session slot",
				"session_key", sessionKey, "waited", liveSlotWaitMax)
			return false
		case <-ticker.C:
			if e.isLiveSession(sessionKey) || e.liveSlotAvailable(sessionKey) {
				return true
			}
		}
	}
}
