package core

import (
	"strings"
	"testing"
	"time"
)

func TestIsChainableSessionCmd(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"/new", true},
		{"/new some name", true},
		{"/NEW", true},
		{"new", true},
		{"/next", true},
		{"/next work on issue 42", true},
		{"/switch", true},
		{"/switch 3", true},
		{"/stop", false},
		{"/delete 1", false},
		{"/list", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isChainableSessionCmd(tc.cmd); got != tc.want {
			t.Errorf("isChainableSessionCmd(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestExecuteSessionCmdAndSend_RejectsEmptyPrompt(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	err := e.ExecuteSessionCmdAndSend("test:u1", "/new", "   ")
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestExecuteSessionCmdAndSend_RejectsUnchainableCommand(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	err := e.ExecuteSessionCmdAndSend("test:u1", "/stop", "do a thing")
	if err == nil {
		t.Fatal("expected error for unchainable command")
	}
}

// TestExecuteSessionCmdAndSend_NewStartsFreshAndDelivers verifies that
// combining /new with a kickoff prompt clears the previous session's
// resumable agent id and feeds the prompt to a freshly spawned agent
// session — the batch-issue handoff use case.
func TestExecuteSessionCmdAndSend_NewStartsFreshAndDelivers(t *testing.T) {
	p := &reconstructingPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	agentSession := newResultAgentSession("done")
	agent := &resultAgent{session: agentSession}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:u1"
	old := e.sessions.GetOrCreateActive(key)
	old.AddHistory("user", "earlier turn")
	old.SetAgentSessionID("old-agent-session", "stub")

	if err := e.ExecuteSessionCmdAndSend(key, "/new", "Work on issue #42"); err != nil {
		t.Fatalf("ExecuteSessionCmdAndSend: %v", err)
	}

	// The new active session must not be the old one.
	deadline := time.After(2 * time.Second)
	var prompts []string
	for {
		active := e.sessions.GetOrCreateActive(key)
		prompts = agentSession.SentPrompts()
		if active.ID != old.ID && len(prompts) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: active=%s old=%s prompts=%v",
				e.sessions.GetOrCreateActive(key).ID, old.ID, prompts)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := old.GetAgentSessionID(); got != "" {
		t.Errorf("old session agent id = %q, want cleared", got)
	}
	found := false
	for _, prompt := range prompts {
		if prompt == "Work on issue #42" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("agent did not receive kickoff prompt; got = %v", prompts)
	}
}

type reconstructingPlatform struct {
	stubPlatformEngine
}

func (p *reconstructingPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "reconstructed:" + sessionKey, nil
}

func TestCmdNext_WithoutPromptShowsUsage(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)

	msg := &Message{
		SessionKey: "test:u1",
		Platform:   "test",
		UserID:     "u1",
		UserName:   "user",
		Content:    "/next",
		ReplyCtx:   "ctx",
	}
	e.handleCommand(p, msg, "/next")

	sent := p.getSent()
	if len(sent) == 0 {
		t.Fatal("expected usage reply, got none")
	}
	wantFragment := "/next"
	foundUsage := false
	for _, s := range sent {
		if strings.Contains(s, wantFragment) {
			foundUsage = true
			break
		}
	}
	if !foundUsage {
		t.Errorf("expected usage reply mentioning %q, got %v", wantFragment, sent)
	}
}

func TestCmdNext_WithPromptStartsNewSessionAndDelivers(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	agentSession := newResultAgentSession("ok")
	agent := &resultAgent{session: agentSession}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	key := "test:u1"
	old := e.sessions.GetOrCreateActive(key)
	old.SetAgentSessionID("old-agent", "stub")

	msg := &Message{
		SessionKey: key,
		Platform:   "test",
		UserID:     "u1",
		UserName:   "user",
		Content:    "/next fix the login bug",
		ReplyCtx:   "ctx",
	}
	e.handleCommand(p, msg, "/next fix the login bug")

	deadline := time.After(2 * time.Second)
	var prompts []string
	for {
		active := e.sessions.GetOrCreateActive(key)
		prompts = agentSession.SentPrompts()
		if active.ID != old.ID && len(prompts) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: prompts=%v", prompts)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := old.GetAgentSessionID(); got != "" {
		t.Errorf("old session agent id = %q, want cleared", got)
	}
	if got := prompts[0]; got != "fix the login bug" {
		t.Errorf("agent prompt = %q, want %q", got, "fix the login bug")
	}
}

