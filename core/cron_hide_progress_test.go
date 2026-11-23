package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// hpCard is a StreamingCard + StreamingCardDiscarder that records every
// rendered payload, the final content and whether it was discarded.
type hpCard struct {
	mu        sync.Mutex
	updates   []string
	final     string
	finalized bool
	discards  int
}

func (c *hpCard) Update(_ context.Context, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, content)
	return nil
}
func (c *hpCard) Finalize(_ context.Context, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalized = true
	c.final = content
	return nil
}
func (c *hpCard) Failed() bool { return false }
func (c *hpCard) Discard(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.discards++
	return nil
}
func (c *hpCard) snapshot() (updates []string, final string, finalized bool, discards int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.updates...), c.final, c.finalized, c.discards
}

type hpPlatform struct {
	stubPlatformEngine
	card *hpCard
}

func (p *hpPlatform) CreateStreamingCard(_ context.Context, _ any) (StreamingCard, error) {
	return p.card, nil
}
func (p *hpPlatform) ReconstructReplyCtx(_ string) (any, error) { return "rctx", nil }

func runHPTurn(t *testing.T, fromCron, hideProgress bool, events ...Event) *hpCard {
	t.Helper()
	card := &hpCard{}
	p := &hpPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}, card: card}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.cancel()
	sessionKey := "slack:C-hp:posthog-feed"
	session := e.sessions.GetOrCreateActive(sessionKey)
	as := newControllableSession("s-hp")
	state := &interactiveState{
		agentSession:     as,
		platform:         p,
		replyCtx:         "rctx",
		turnFromCron:     fromCron,
		turnHideProgress: hideProgress,
	}
	e.interactiveStates[sessionKey] = state
	for _, ev := range events {
		as.events <- ev
	}
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "m-hp", time.Now(), nil, nil, state.replyCtx, telemetryMsgCtx{})
	if state.turnFromCron || state.turnHideProgress {
		t.Fatalf("per-turn flags must be consumed by the turn (fromCron=%v hide=%v)", state.turnFromCron, state.turnHideProgress)
	}
	return card
}

func containsThinking(ss ...string) bool {
	for _, s := range ss {
		if strings.Contains(s, "Thinking") || strings.Contains(s, "pondering the alert") {
			return true
		}
	}
	return false
}

// (a) hide_progress: no thinking preview ever rendered, only the answer.
func TestHideProgress_CronTurnPostsOnlyFinalAnswer(t *testing.T) {
	card := runHPTurn(t, true, true,
		Event{Type: EventThinking, Content: "pondering the alert"},
		Event{Type: EventText, Content: "Real alert: checkout errors up 5x"},
		Event{Type: EventResult, Content: "Real alert: checkout errors up 5x", Done: true},
	)
	updates, final, finalized, _ := card.snapshot()
	if containsThinking(updates...) || containsThinking(final) {
		t.Fatalf("thinking leaked with hide_progress: updates=%q final=%q", updates, final)
	}
	if !finalized || !strings.Contains(final, "Real alert: checkout errors up 5x") {
		t.Fatalf("final answer not delivered: finalized=%v final=%q", finalized, final)
	}
}

// (a)+(b) hide_progress with NO_REPLY: nothing rendered at all.
func TestHideProgress_CronNoReplyPostsNothing(t *testing.T) {
	card := runHPTurn(t, true, true,
		Event{Type: EventThinking, Content: "pondering the alert"},
		Event{Type: EventText, Content: "NO_REPLY"},
		Event{Type: EventResult, Content: "NO_REPLY", Done: true},
	)
	updates, final, _, _ := card.snapshot()
	if len(updates) != 0 {
		t.Fatalf("expected no card updates, got %q", updates)
	}
	if final != "" || containsThinking(final) {
		t.Fatalf("expected nothing rendered, final=%q", final)
	}
}

// (b) cron run without hide_progress that resolves to NO_REPLY: the
// thinking-only card is removed instead of being left behind.
func TestCronNoReply_DiscardsThinkingOnlyCard(t *testing.T) {
	card := runHPTurn(t, true, false,
		Event{Type: EventThinking, Content: "pondering the alert"},
		Event{Type: EventText, Content: "NO_REPLY"},
		Event{Type: EventResult, Content: "NO_REPLY", Done: true},
	)
	updates, _, finalized, discards := card.snapshot()
	if !containsThinking(updates...) {
		t.Fatalf("expected thinking preview while running (hide_progress off), got %q", updates)
	}
	if discards != 1 {
		t.Fatalf("discards = %d, want 1", discards)
	}
	if finalized {
		t.Fatalf("card must not be finalized (re-posted) after discard")
	}
}

// Control (#product): a normal user turn keeps thinking previews and the
// existing NO_REPLY behaviour (card finalized, not deleted).
func TestUserTurn_KeepsThinkingAndDoesNotDiscard(t *testing.T) {
	card := runHPTurn(t, false, false,
		Event{Type: EventThinking, Content: "pondering the alert"},
		Event{Type: EventText, Content: "NO_REPLY"},
		Event{Type: EventResult, Content: "NO_REPLY", Done: true},
	)
	updates, final, finalized, discards := card.snapshot()
	if !containsThinking(updates...) || !containsThinking(final) {
		t.Fatalf("user turn must keep thinking preview: updates=%q final=%q", updates, final)
	}
	if discards != 0 || !finalized {
		t.Fatalf("user turn: discards=%d finalized=%v, want 0/true", discards, finalized)
	}

	card = runHPTurn(t, false, false,
		Event{Type: EventThinking, Content: "pondering the alert"},
		Event{Type: EventText, Content: "answer"},
		Event{Type: EventResult, Content: "answer", Done: true},
	)
	_, final, _, _ = card.snapshot()
	if !containsThinking(final) || !strings.Contains(final, "answer") {
		t.Fatalf("user turn final card should hold thinking + answer, got %q", final)
	}
}

func TestCronJob_HideProgressJSON(t *testing.T) {
	var j CronJob
	if err := json.Unmarshal([]byte(`{"id":"e8c0b8c7","hide_progress":true}`), &j); err != nil {
		t.Fatal(err)
	}
	if !j.HideProgress {
		t.Fatal("hide_progress not parsed")
	}
	out, _ := json.Marshal(CronJob{ID: "x"})
	if strings.Contains(string(out), "hide_progress") {
		t.Fatalf("hide_progress should be omitted when false: %s", out)
	}
}

// thinkingResultSession emits a thinking event, then the result, on Send.
type thinkingResultSession struct {
	*resultAgentSession
	thinking string
}

func (s *thinkingResultSession) Send(prompt string, id string, imgs []ImageAttachment, files []FileAttachment) error {
	s.sendOnce.Do(func() {
		s.events <- Event{Type: EventThinking, Content: s.thinking}
		s.events <- Event{Type: EventText, Content: s.result}
		s.events <- Event{Type: EventResult, Content: s.result, Done: true}
	})
	s.mu.Lock()
	s.sentPrompts = append(s.sentPrompts, prompt)
	s.mu.Unlock()
	return nil
}

// End to end through ExecuteCronJob: the job's hide_progress reaches the turn.
func TestExecuteCronJob_HideProgressPlumbed(t *testing.T) {
	for _, hide := range []bool{true, false} {
		card := &hpCard{}
		p := &hpPlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}, card: card}
		rs := newResultAgentSession("triaged: real alert")
		rs.events = make(chan Event, 4)
		as := &thinkingResultSession{resultAgentSession: rs, thinking: "pondering the alert"}
		e := NewEngine("test", &resultAgent{session: as}, []Platform{p}, "", LangEnglish)
		job := &CronJob{ID: "e8c0b8c7", SessionKey: "slack:C-hp:posthog-feed", Prompt: "triage", SessionMode: "new_per_run", HideProgress: hide}
		if err := e.ExecuteCronJob(job); err != nil {
			t.Fatalf("hide=%v ExecuteCronJob: %v", hide, err)
		}
		e.cancel()
		updates, final, _, _ := card.snapshot()
		leaked := containsThinking(updates...) || containsThinking(final)
		if hide && leaked {
			t.Fatalf("hide_progress=true leaked thinking: updates=%q final=%q", updates, final)
		}
		if !hide && !leaked {
			t.Fatalf("hide_progress=false should show thinking: updates=%q final=%q", updates, final)
		}
		if !strings.Contains(final, "triaged: real alert") {
			t.Fatalf("hide=%v final answer missing: %q", hide, final)
		}
	}
}
