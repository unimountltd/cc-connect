package core

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// wsModelAgent is a workspace-agent stub that records the options it was
// constructed with, so tests can assert what getOrCreateWorkspaceAgent passed.
type wsModelAgent struct {
	stubModelModeAgent
	name string
	opts map[string]any
}

func (a *wsModelAgent) Name() string { return a.name }
func (a *wsModelAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return &stubAgentSession{}, nil
}
func (a *wsModelAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *wsModelAgent) Stop() error { return nil }

// newWorkspaceModelEngine builds a multi-workspace engine whose agent factory
// honours opts["model"]/opts["reasoning_effort"], plus a project state store.
// created collects every workspace agent the engine builds.
func newWorkspaceModelEngine(t *testing.T, agentName string) (*Engine, *ProjectStateStore, *[]*wsModelAgent, *sync.Mutex) {
	t.Helper()

	var mu sync.Mutex
	created := make([]*wsModelAgent, 0, 2)

	RegisterAgent(agentName, func(opts map[string]any) (Agent, error) {
		model, _ := opts["model"].(string)
		effort, _ := opts["reasoning_effort"].(string)
		a := &wsModelAgent{name: agentName, opts: opts}
		a.model = model
		a.reasoningEffort = effort
		mu.Lock()
		created = append(created, a)
		mu.Unlock()
		return a, nil
	})

	base := &wsModelAgent{name: agentName}
	base.model = "fable"
	base.reasoningEffort = "high"

	tmp := t.TempDir()
	e := NewEngine("test", base, []Platform{&stubPlatformEngine{n: "plain"}},
		filepath.Join(tmp, "sessions.json"), LangEnglish)
	e.SetMultiWorkspace(t.TempDir(), filepath.Join(tmp, "bindings.json"))

	store := NewProjectStateStore(filepath.Join(tmp, "project_state.json"))
	e.SetProjectStateStore(store)

	return e, store, &created, &mu
}

func TestProjectStateStore_WorkspaceModelSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project_state.json")

	store := NewProjectStateStore(path)
	if _, ok := store.WorkspaceModel("/ws/a"); ok {
		t.Fatal("expected no stored pref for a fresh store")
	}
	store.SetWorkspaceModel("/ws/a", WorkspaceModelPref{Model: "opus", Effort: "high"})
	store.Save()

	reloaded := NewProjectStateStore(path)
	pref, ok := reloaded.WorkspaceModel("/ws/a")
	if !ok {
		t.Fatal("expected pref to survive reload")
	}
	if pref.Model != "opus" || pref.Effort != "high" {
		t.Fatalf("pref = %+v, want {opus high}", pref)
	}

	reloaded.ClearWorkspaceModel("/ws/a")
	reloaded.Save()
	if _, ok := NewProjectStateStore(path).WorkspaceModel("/ws/a"); ok {
		t.Fatal("expected pref to be cleared")
	}
}

func TestProjectStateStore_SetWorkspaceModelEmptyClears(t *testing.T) {
	store := NewProjectStateStore("")
	store.SetWorkspaceModel("/ws/a", WorkspaceModelPref{Model: "opus"})
	store.SetWorkspaceModel("/ws/a", WorkspaceModelPref{})
	if _, ok := store.WorkspaceModel("/ws/a"); ok {
		t.Fatal("an empty pref should clear the entry, not store it")
	}
}

// Regression: a model chosen for a workspace must outlive that workspace's
// agent. The pool reaps idle workspaces every 15 minutes and the whole pool is
// empty after a restart, and before this the rebuilt agent silently fell back
// to the project-level config default.
func TestGetOrCreateWorkspaceAgent_RestoresPersistedModel(t *testing.T) {
	e, store, created, mu := newWorkspaceModelEngine(t, "ws-model-restore-agent")

	wsDir := normalizeWorkspacePath(t.TempDir())
	store.SetWorkspaceModel(wsDir, WorkspaceModelPref{Model: "opus", Effort: "max"})
	store.Save()

	agent, _, err := e.getOrCreateWorkspaceAgent(wsDir)
	if err != nil {
		t.Fatalf("getOrCreateWorkspaceAgent: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*created) != 1 {
		t.Fatalf("created %d workspace agents, want 1", len(*created))
	}
	built := (*created)[0]
	if built != agent {
		t.Fatal("returned agent is not the one the factory built")
	}
	if got := built.opts["model"]; got != "opus" {
		t.Fatalf("opts[model] = %v, want opus (project default fable must not win)", got)
	}
	if got := built.opts["reasoning_effort"]; got != "max" {
		t.Fatalf("opts[reasoning_effort] = %v, want max", got)
	}
}

func TestGetOrCreateWorkspaceAgent_NoPrefKeepsProjectDefault(t *testing.T) {
	e, _, created, mu := newWorkspaceModelEngine(t, "ws-model-default-agent")

	if _, _, err := e.getOrCreateWorkspaceAgent(normalizeWorkspacePath(t.TempDir())); err != nil {
		t.Fatalf("getOrCreateWorkspaceAgent: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := (*created)[0].opts["model"]; got != "fable" {
		t.Fatalf("opts[model] = %v, want the project default fable", got)
	}
}

// End-to-end: switching the model in a bound channel, then losing the
// workspace agent to the idle reaper, must not lose the selection.
func TestModelSwitch_SurvivesWorkspaceReap(t *testing.T) {
	e, store, created, mu := newWorkspaceModelEngine(t, "ws-model-reap-agent")

	wsDir := normalizeWorkspacePath(t.TempDir())
	channelID := "channel1"
	sessionKey := "plain:" + channelID + ":user1"
	e.workspaceBindings.Bind("project:test", channelID, "chan", wsDir)

	agent, _, workspace := e.workspaceContextForKey(sessionKey)
	if workspace != wsDir {
		t.Fatalf("workspace = %q, want %q", workspace, wsDir)
	}
	if agent == e.agent {
		t.Fatal("expected a workspace-specific agent")
	}

	e.interactiveMu.Lock()
	e.interactiveStates[e.interactiveKeyForSessionKey(sessionKey)] = &interactiveState{}
	e.interactiveMu.Unlock()

	e.executeCardAction("/model", "opus", sessionKey)
	waitForInteractiveCleanup(t, e, e.interactiveKeyForSessionKey(sessionKey))

	pref, ok := store.WorkspaceModel(wsDir)
	if !ok || pref.Model != "opus" {
		t.Fatalf("persisted pref = %+v (ok=%v), want model opus", pref, ok)
	}

	// Simulate the 15-minute idle reap dropping the workspace agent.
	e.workspacePool.mu.Lock()
	delete(e.workspacePool.states, wsDir)
	e.workspacePool.mu.Unlock()

	rebuilt, _, _ := e.workspaceContextForKey(sessionKey)
	if rebuilt == agent {
		t.Fatal("expected a freshly built workspace agent after the reap")
	}

	mu.Lock()
	defer mu.Unlock()
	last := (*created)[len(*created)-1]
	if got := last.opts["model"]; got != "opus" {
		t.Fatalf("rebuilt agent opts[model] = %v, want opus", got)
	}
	if got := last.GetModel(); got != "opus" {
		t.Fatalf("rebuilt agent model = %q, want opus", got)
	}
}

// Regression: switchModelOnAgent used to call modelSaveFunc regardless of
// persistConfig whenever the agent had no active provider — the normal case
// for Claude Code. A per-workspace switch therefore rewrote config.toml's
// project-level default, so every other workspace inherited that model the
// next time its agent was rebuilt.
func TestSwitchModelOnAgent_WorkspaceSwitchDoesNotRewriteConfigDefault(t *testing.T) {
	e, _, _, _ := newWorkspaceModelEngine(t, "ws-model-nosave-agent")

	var saved []string
	e.modelSaveFunc = func(model string) error {
		saved = append(saved, model)
		return nil
	}

	wsAgent := &wsModelAgent{name: "ws-model-nosave-agent"}
	if _, err := e.switchModelOnAgent(wsAgent, "opus", false); err != nil {
		t.Fatalf("switchModelOnAgent: %v", err)
	}
	if len(saved) != 0 {
		t.Fatalf("workspace switch wrote config default %v, want no write", saved)
	}
	if wsAgent.GetModel() != "opus" {
		t.Fatalf("workspace agent model = %q, want opus", wsAgent.GetModel())
	}

	if _, err := e.switchModelOnAgent(e.agent, "sonnet", true); err != nil {
		t.Fatalf("switchModelOnAgent: %v", err)
	}
	if len(saved) != 1 || saved[0] != "sonnet" {
		t.Fatalf("project-level switch saved %v, want [sonnet]", saved)
	}
}

// The project-level agent already persists its model to config.toml, so a
// non-workspace switch must not write a workspace entry.
func TestPersistWorkspaceModel_SkipsProjectLevelAgent(t *testing.T) {
	e, store, _, _ := newWorkspaceModelEngine(t, "ws-model-skip-agent")

	e.persistWorkspaceModel("", e.agent)
	e.persistWorkspaceModel("/ws/a", e.agent)

	if _, ok := store.WorkspaceModel("/ws/a"); ok {
		t.Fatal("project-level agent must not write a per-workspace pref")
	}
}
