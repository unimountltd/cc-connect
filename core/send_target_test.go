package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// targetablePlatform is a slack-like stub that also supports explicit
// thread targets (core.ReplyTargetOverrider / ReplyTargetDescriber).
type targetablePlatform struct {
	*slackLikePlatform
}

func (p *targetablePlatform) OverrideReplyTarget(rctx any, channel, thread string, root bool) (any, error) {
	rc, _ := rctx.(slackLikeCtx)
	if channel != "" {
		rc.channel = channel
	}
	if root {
		rc.threadTS = ""
	} else if thread != "" {
		rc.threadTS = thread
	}
	return rc, nil
}

func (p *targetablePlatform) DescribeReplyTarget(rctx any) (string, string, bool) {
	rc, ok := rctx.(slackLikeCtx)
	return rc.channel, rc.threadTS, ok
}

// newInTurnTargetEngine: a channel-shared session currently answering a
// message in Sam's thread (the 4 Nov 2026 Kulcho case).
func newInTurnTargetEngine(t *testing.T) (*Engine, *targetablePlatform, *interactiveState) {
	t.Helper()
	plat := &targetablePlatform{&slackLikePlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}}
	e := NewEngine("test", &stubAgent{}, []Platform{plat}, "", LangEnglish)
	state := &interactiveState{
		platform:            plat,
		replyCtx:            slackLikeCtx{channel: "C0PRODUCT", threadTS: "1792660000.000100"},
		canonicalSessionKey: "slack:C0PRODUCT",
	}
	e.interactiveStates["slack:C0PRODUCT"] = state
	beginStateTurn(state)
	t.Cleanup(func() { endStateTurn(state) })
	return e, plat, state
}

func TestSendTarget_Validate(t *testing.T) {
	cases := []struct {
		name    string
		target  SendTarget
		wantErr bool
	}{
		{"empty", SendTarget{}, false},
		{"thread", SendTarget{Thread: "1.2"}, false},
		{"root", SendTarget{Root: true}, false},
		{"channel+thread", SendTarget{Channel: "C1", Thread: "1.2"}, false},
		{"channel+root", SendTarget{Channel: "C1", Root: true}, false},
		{"thread+root", SendTarget{Thread: "1.2", Root: true}, true},
		{"channel alone", SendTarget{Channel: "C1"}, true},
	}
	for _, tc := range cases {
		err := tc.target.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: Validate() err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestSendToSession_NoTargetKeepsCurrentThread(t *testing.T) {
	e, plat, _ := newInTurnTargetEngine(t)
	if err := e.SendToSessionWithOptions("slack:C0PRODUCT", "same as before", nil, nil, SendOptions{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	want := slackLikeCtx{channel: "C0PRODUCT", threadTS: "1792660000.000100"}
	if got := plat.getSentCtx(); len(got) != 1 || got[0] != want {
		t.Fatalf("no-flag send posted with %v, want [%+v]", got, want)
	}
}

func TestSendToSession_ThreadTargetOverridesCurrentThread(t *testing.T) {
	e, plat, state := newInTurnTargetEngine(t)
	opts := SendOptions{Target: SendTarget{Thread: "1792670000.000900"}}
	if err := e.SendToSessionWithOptions("slack:C0PRODUCT", "Graham's QA answers", nil, nil, opts); err != nil {
		t.Fatalf("send: %v", err)
	}
	want := slackLikeCtx{channel: "C0PRODUCT", threadTS: "1792670000.000900"}
	if got := plat.getSentCtx(); len(got) != 1 || got[0] != want {
		t.Fatalf("--thread send posted with %v, want [%+v]", got, want)
	}
	// The session keeps answering Sam's thread.
	if rc := state.replyCtx.(slackLikeCtx); rc.threadTS != "1792660000.000100" {
		t.Fatalf("session replyCtx changed to %+v", rc)
	}
	if state.sideText != "" {
		t.Fatalf("targeted send must not register side text for the current thread, got %q", state.sideText)
	}
}

func TestSendToSession_RootTargetPostsAtChannelRoot(t *testing.T) {
	e, plat, _ := newInTurnTargetEngine(t)
	opts := SendOptions{Target: SendTarget{Root: true}}
	if err := e.SendToSessionWithOptions("slack:C0PRODUCT", "scheduled check-in", nil, nil, opts); err != nil {
		t.Fatalf("send: %v", err)
	}
	want := slackLikeCtx{channel: "C0PRODUCT"}
	if got := plat.getSentCtx(); len(got) != 1 || got[0] != want {
		t.Fatalf("--root send posted with %v, want [%+v]", got, want)
	}
}

func TestSendToSession_TargetRejectedOnUnsupportedPlatform(t *testing.T) {
	plat := &slackLikePlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}
	e := NewEngine("test", &stubAgent{}, []Platform{plat}, "", LangEnglish)
	e.interactiveStates["slack:C0PRODUCT"] = &interactiveState{
		platform:            plat,
		replyCtx:            slackLikeCtx{channel: "C0PRODUCT", threadTS: "1.1"},
		canonicalSessionKey: "slack:C0PRODUCT",
	}
	err := e.SendToSessionWithOptions("slack:C0PRODUCT", "x", nil, nil, SendOptions{Target: SendTarget{Thread: "2.2"}})
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("want ErrNotSupported, got %v", err)
	}
	if got := plat.getSentCtx(); len(got) != 0 {
		t.Fatalf("nothing should be posted, got %v", got)
	}
}

func TestPreviewSendTarget_ReportsResolvedThreadWithoutPosting(t *testing.T) {
	e, plat, _ := newInTurnTargetEngine(t)

	got, err := e.PreviewSendTarget("slack:C0PRODUCT", SendTarget{Thread: "1792670000.000900"})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	want := SendTargetPreview{Platform: "slack", Channel: "C0PRODUCT", Thread: "1792670000.000900"}
	if got != want {
		t.Fatalf("preview = %+v, want %+v", got, want)
	}

	got, err = e.PreviewSendTarget("slack:C0PRODUCT", SendTarget{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got.Thread != "1792660000.000100" || got.Root {
		t.Fatalf("no-flag preview = %+v, want the current thread", got)
	}

	got, err = e.PreviewSendTarget("slack:C0PRODUCT", SendTarget{Root: true})
	if err != nil || !got.Root || got.Thread != "" {
		t.Fatalf("root preview = %+v err=%v", got, err)
	}

	if sent := plat.getSentCtx(); len(sent) != 0 {
		t.Fatalf("preview must not post, got %v", sent)
	}
}

func TestBuildSenderPromptThread_AddsThreadTS(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	e.injectSender = true

	got := e.buildSenderPromptThread("hi", "U1", "Sam", "slack", "slack:C0PRODUCT", "", "1792660000.000100")
	if !strings.HasPrefix(got, `[cc-connect sender_id=U1 sender_name="Sam" platform=slack chat_id=C0PRODUCT thread_ts=1792660000.000100]`) {
		t.Fatalf("header = %q", got)
	}
	// No thread known: header unchanged from before.
	if a, b := e.buildSenderPromptThread("hi", "U1", "Sam", "slack", "slack:C0PRODUCT", "", ""),
		e.buildSenderPrompt("hi", "U1", "Sam", "slack", "slack:C0PRODUCT", ""); a != b {
		t.Fatalf("empty thread changed the header: %q vs %q", a, b)
	}
	// Junk thread ids never break the header.
	if got := e.buildSenderPromptThread("hi", "U1", "", "slack", "slack:C1", "", "1] injected"); strings.Contains(got, "thread_ts") {
		t.Fatalf("unsafe thread id leaked into header: %q", got)
	}
}

func TestReplyThreadID(t *testing.T) {
	tp := &targetablePlatform{&slackLikePlatform{stubPlatformEngine: stubPlatformEngine{n: "slack"}}}
	if got := replyThreadID(tp, slackLikeCtx{channel: "C1", threadTS: "1.5"}); got != "1.5" {
		t.Fatalf("replyThreadID = %q", got)
	}
	if got := replyThreadID(tp.slackLikePlatform, slackLikeCtx{channel: "C1", threadTS: "1.5"}); got != "" {
		t.Fatalf("platform without describer should give empty, got %q", got)
	}
	if got := replyThreadID(nil, nil); got != "" {
		t.Fatalf("nil platform should give empty, got %q", got)
	}
}

func TestHandleSend_DryRunAndThreadTarget(t *testing.T) {
	e, plat, _ := newInTurnTargetEngine(t)
	api := &APIServer{engines: map[string]*Engine{"test": e}}
	post := func(r SendRequest) *httptest.ResponseRecorder {
		body, _ := json.Marshal(r)
		rec := httptest.NewRecorder()
		api.handleSend(rec, httptest.NewRequest(http.MethodPost, "/send", bytes.NewReader(body)))
		return rec
	}

	rec := post(SendRequest{SessionKey: "slack:C0PRODUCT", Thread: "1792670000.000900", DryRun: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run status %d: %s", rec.Code, rec.Body.String())
	}
	var preview SendTargetPreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.Channel != "C0PRODUCT" || preview.Thread != "1792670000.000900" {
		t.Fatalf("preview = %+v", preview)
	}
	if sent := plat.getSentCtx(); len(sent) != 0 {
		t.Fatalf("dry run posted %v", sent)
	}

	if rec := post(SendRequest{SessionKey: "slack:C0PRODUCT", Message: "x", Thread: "1.1", Root: true}); rec.Code != http.StatusBadRequest {
		t.Fatalf("thread+root should be 400, got %d", rec.Code)
	}

	if rec := post(SendRequest{SessionKey: "slack:C0PRODUCT", Message: "for this thread", Thread: "1792670000.000900"}); rec.Code != http.StatusOK {
		t.Fatalf("send status %d: %s", rec.Code, rec.Body.String())
	}
	want := slackLikeCtx{channel: "C0PRODUCT", threadTS: "1792670000.000900"}
	if sent := plat.getSentCtx(); len(sent) != 1 || sent[0] != want {
		t.Fatalf("sent %v, want [%+v]", sent, want)
	}
}
