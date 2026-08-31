package core

import "testing"

// stubPresetAgent extends the model/mode stub with named presets so /preset
// can drive its ModelSwitcher + ReasoningEffortSwitcher together.
type stubPresetAgent struct {
	stubModelModeAgent
	presets []ModePreset
}

func (a *stubPresetAgent) AvailablePresets() []ModePreset {
	if a.presets == nil {
		return []ModePreset{
			{Name: "fable", Model: "fable", Effort: "high", Desc: "Fable + high"},
			{Name: "opus", Model: "opus", Effort: "high", Desc: "Opus + high"},
		}
	}
	return a.presets
}

func TestCmdPreset_ByNameSetsModelAndEffort(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	agent := &stubPresetAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	s := e.sessions.GetOrCreateActive(msg.SessionKey)
	s.SetAgentSessionID("existing-session", "test")

	e.cmdPreset(p, msg, []string{"opus"})

	if agent.model != "opus" {
		t.Fatalf("agent model = %q, want opus", agent.model)
	}
	if agent.reasoningEffort != "high" {
		t.Fatalf("agent effort = %q, want high", agent.reasoningEffort)
	}
	if active := e.sessions.GetOrCreateActive(msg.SessionKey); active.AgentSessionID != "" {
		t.Fatalf("session id = %q, want cleared after preset switch", active.AgentSessionID)
	}
}

// Regression: the upstream merge kept cmdPreset but dropped "preset" from
// builtinCommands, so real platform messages fell through to the agent as an
// unknown slash command. Drive the public platform entrypoint to cover command
// resolution as well as the implementation.
func TestPresetCommand_RoutesThroughReceiveMessage(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	agent := &stubPresetAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.ReceiveMessage(p, &Message{
		SessionKey: "plain:chat:user1",
		Platform:   "plain",
		UserID:     "user1",
		ReplyCtx:   "ctx",
		Content:    "/preset opus",
	})

	if agent.model != "opus" || agent.reasoningEffort != "high" {
		t.Fatalf("/preset opus => model=%q effort=%q, want opus/high", agent.model, agent.reasoningEffort)
	}
	if sent := p.getSent(); len(sent) == 0 || sent[len(sent)-1] != e.i18n.Tf(MsgPresetChanged, "opus", "opus", "high") {
		t.Fatalf("/preset reply = %v, want preset confirmation", sent)
	}
}

func TestCmdPreset_ByNumberSelectsPreset(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	agent := &stubPresetAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	msg := &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}

	e.cmdPreset(p, msg, []string{"1"})

	if agent.model != "fable" || agent.reasoningEffort != "high" {
		t.Fatalf("preset 1 => model=%q effort=%q, want fable/high", agent.model, agent.reasoningEffort)
	}
}

func TestCmdPreset_ListShowsButtons(t *testing.T) {
	p := &stubInlineButtonPlatform{stubPlatformEngine: stubPlatformEngine{n: "inline-only"}}
	agent := &stubPresetAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.cmdPreset(p, &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}, nil)

	if len(p.buttonRows) == 0 {
		t.Fatal("expected /preset to send inline buttons")
	}
	if got := p.buttonRows[0][0].Data; got != "cmd:/preset 1" {
		t.Fatalf("first /preset button = %q, want cmd:/preset 1", got)
	}
}

func TestCmdPreset_UnknownNameShowsUsage(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	agent := &stubPresetAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.cmdPreset(p, &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}, []string{"nope"})

	if agent.model != "" || agent.reasoningEffort != "" {
		t.Fatalf("unknown preset should not change state: model=%q effort=%q", agent.model, agent.reasoningEffort)
	}
	sent := p.getSent()
	if len(sent) == 0 || sent[len(sent)-1] != e.i18n.T(MsgPresetUsage) {
		t.Fatalf("expected usage hint, got %v", sent)
	}
}

func TestMatchesPresetName(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	agent := &stubPresetAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	cases := map[string]bool{
		"fable":     true,
		"OPUS":      true, // case-insensitive
		"  opus  ":  true, // trimmed
		"fabl":      false,
		"use fable": false, // must be exact, not a substring
		"":          false,
	}
	for in, want := range cases {
		if got := e.matchesPresetName(in); got != want {
			t.Errorf("matchesPresetName(%q) = %v, want %v", in, got, want)
		}
	}

	// Agents without preset support never match.
	e2 := NewEngine("test", &stubModelModeAgent{}, []Platform{p}, "", LangEnglish)
	if e2.matchesPresetName("fable") {
		t.Fatal("non-preset agent should not match preset names")
	}
}

func TestCmdPreset_NotSupported(t *testing.T) {
	p := &stubPlatformEngine{n: "plain"}
	// stubModelModeAgent has no AvailablePresets → not a PresetSwitcher.
	agent := &stubModelModeAgent{}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	e.cmdPreset(p, &Message{SessionKey: "test:user1", ReplyCtx: "ctx"}, []string{"opus"})

	sent := p.getSent()
	if len(sent) == 0 || sent[len(sent)-1] != e.i18n.T(MsgPresetNotSupported) {
		t.Fatalf("expected not-supported message, got %v", sent)
	}
}
