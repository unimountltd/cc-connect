package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/slack-go/slack"
)

func TestStreamingCard_DiscardDeletesPostedCard(t *testing.T) {
	var postCalls, deleteCalls int32
	var deletedTS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			atomic.AddInt32(&postCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C1", "ts": "1700000000.0001"})
		case "/chat.delete":
			atomic.AddInt32(&deleteCalls, 1)
			_ = r.ParseForm()
			deletedTS = r.FormValue("ts")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C1", "ts": deletedTS})
		default:
			t.Fatalf("unexpected slack API path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	client := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))

	// Never-posted card: Discard is a no-op (no API call).
	lazy := &slackStreamingCard{client: client, channel: "C1"}
	if err := lazy.Discard(context.Background()); err != nil {
		t.Fatalf("Discard on unposted card: %v", err)
	}
	if atomic.LoadInt32(&deleteCalls) != 0 {
		t.Fatal("Discard on an unposted card must not call chat.delete")
	}

	card := &slackStreamingCard{client: client, channel: "C1"}
	if err := card.Update(context.Background(), "💭 **Thinking**\n\nhmm"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := card.Discard(context.Background()); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got := atomic.LoadInt32(&deleteCalls); got != 1 || deletedTS != "1700000000.0001" {
		t.Fatalf("chat.delete calls=%d ts=%q, want 1 / 1700000000.0001", got, deletedTS)
	}
	if card.ts != "" {
		t.Fatal("card.ts should be cleared after Discard")
	}
}
