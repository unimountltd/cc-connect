package claudecode

import (
	"context"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// TestSession_InTurnRoutingHappyPath verifies that events emitted while a
// Send() turn is in flight land on cs.events (the active channel), and
// events emitted after EventResult clears inTurn land on cs.orphanEvents.
// This is the load-bearing property the orphan handler depends on — if it
// breaks, normal user turns leak into the orphan channel and orphan turns
// leak into the active loop.
func TestSession_InTurnRoutingHappyPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs := &claudeSession{
		events:       make(chan core.Event, 8),
		orphanEvents: make(chan core.Event, 8),
		ctx:          ctx,
	}
	cs.sessionID.Store("s1")
	cs.alive.Store(true)

	// Active turn: simulate Send() setting inTurn=true.
	cs.inTurn.Store(true)
	cs.handleAssistant(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "hello from turn"},
			},
		},
	})
	select {
	case evt := <-cs.events:
		if evt.Content != "hello from turn" {
			t.Fatalf("active event content = %q", evt.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("active event not delivered to cs.events")
	}
	if got := drainOne(cs.orphanEvents); got != nil {
		t.Fatalf("active turn event leaked to orphan channel: %+v", got)
	}

	// EventResult clears inTurn.
	cs.handleResult(map[string]any{"type": "result", "result": "done"})
	resultEvt := <-cs.events
	if resultEvt.Type != core.EventResult {
		t.Fatalf("EventResult missing, got %v", resultEvt.Type)
	}
	if cs.inTurn.Load() {
		t.Fatal("inTurn still true after EventResult")
	}

	// Orphan turn: simulate the SDK firing a wakeup with no Send() in flight.
	cs.handleAssistant(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "from wakeup"},
			},
		},
	})
	select {
	case evt := <-cs.orphanEvents:
		if evt.Content != "from wakeup" {
			t.Fatalf("orphan event content = %q", evt.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("orphan event not delivered to cs.orphanEvents")
	}
	if got := drainOne(cs.events); got != nil {
		t.Fatalf("orphan event leaked to active channel: %+v", got)
	}
}

// TestSession_InTurnFallbackWhenOrphanNil verifies that legacy unit tests
// constructing claudeSession by hand (no orphan channel initialized) still
// receive every event on cs.events even when inTurn is false. This is the
// safety net for the dozens of existing handler tests in the package.
func TestSession_InTurnFallbackWhenOrphanNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs := &claudeSession{
		events: make(chan core.Event, 4),
		ctx:    ctx,
		// orphanEvents intentionally nil
	}
	cs.alive.Store(true)
	// inTurn defaults to false, but with orphanEvents nil the event must
	// still go to cs.events (not block forever).
	cs.handleAssistant(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "fallback ok"},
			},
		},
	})
	select {
	case evt := <-cs.events:
		if evt.Content != "fallback ok" {
			t.Fatalf("content = %q", evt.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered (fallback broken)")
	}
}

// TestSession_TurnChannelLockedAtFirstEvent is the regression test for the
// codex P2 finding: a concurrent Send during an in-flight orphan turn used
// to flip cs.inTurn=true and reroute the orphan tail (text + EventResult)
// to cs.events, so the foreground processInteractiveEvents loop consumed
// the wakeup result as if it were the user's reply. Channel-lock at first
// event prevents this.
func TestSession_TurnChannelLockedAtFirstEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs := &claudeSession{
		events:       make(chan core.Event, 8),
		orphanEvents: make(chan core.Event, 8),
		ctx:          ctx,
	}
	cs.alive.Store(true)

	// Orphan turn starts (inTurn=false at the time of first event).
	cs.handleAssistant(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "first orphan event"},
			},
		},
	})

	// Mid-stream, a user Send arrives — flips inTurn=true.
	cs.inTurn.Store(true)

	// Two more events of the SAME orphan turn arrive. Pre-fix these would
	// have been routed to cs.events because emitEvent re-checked inTurn on
	// every event. Post-fix they stay on cs.orphanEvents (channel locked
	// at first event).
	cs.handleAssistant(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "tail of orphan"},
			},
		},
	})
	cs.handleResult(map[string]any{"type": "result", "result": "wakeup done"})

	// All three orphan events should be on cs.orphanEvents.
	wantOrphan := []string{"first orphan event", "tail of orphan", "wakeup done"}
	for i, want := range wantOrphan {
		select {
		case evt := <-cs.orphanEvents:
			got := evt.Content
			if i == 2 && evt.Type != core.EventResult {
				t.Fatalf("orphan terminal: type=%v, want EventResult", evt.Type)
			}
			if got != want {
				t.Fatalf("orphan event %d: content=%q, want %q", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("orphan event %d not delivered (channel-lock regression)", i)
		}
	}
	if got := drainOne(cs.events); got != nil {
		t.Fatalf("orphan tail leaked to active channel: %+v", got)
	}

	// Now that the orphan turn ended (terminal event released turnChannel),
	// the next event re-snapshots — and inTurn=true → routes to cs.events.
	cs.handleAssistant(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "user reply"},
			},
		},
	})
	select {
	case evt := <-cs.events:
		if evt.Content != "user reply" {
			t.Fatalf("user-turn event content = %q", evt.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("user-turn event not delivered after orphan ended")
	}
}

// TestSession_OrphanEventsAccessor returns the channel the engine reads.
func TestSession_OrphanEventsAccessor(t *testing.T) {
	cs := &claudeSession{orphanEvents: make(chan core.Event, 1)}
	if cs.OrphanEvents() == nil {
		t.Fatal("OrphanEvents() returned nil despite initialized channel")
	}
}

// drainOne does a non-blocking read from ch, returning the event or nil.
// Lets tests assert "no event arrived" without using time.After-based
// negative assertions that bloat suite runtime.
func drainOne(ch <-chan core.Event) *core.Event {
	select {
	case evt := <-ch:
		return &evt
	default:
		return nil
	}
}
