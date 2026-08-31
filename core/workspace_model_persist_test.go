package core

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// Upstream persists a workspace's model (WorkspaceModelOverride, #1372). The
// fork adds the reasoning-effort half, so these tests cover effort persistence
// and the combined restore in getOrCreateWorkspaceAgent. Model-only coverage
// lives in upstream's projectstate_test.go / engine_test.go.

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

func TestProjectStateStore_WorkspaceEffortSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project_state.json")

	store := NewProjectStateStore(path)
	if got := store.WorkspaceEffortOverride("/ws/a"); got != "" {
		t.Fatalf("fresh store returned %q, want empty", got)
	}
	store.SetWorkspaceEffortOverride("/ws/a", "max")
	store.Save()

	if got := NewProjectStateStore(path).WorkspaceEffortOverride("/ws/a"); got != "max" {
		t.Fatalf("effort after reload = %q, want max", got)
	}

	reloaded := NewProjectStateStore(path)
	reloaded.ClearWorkspaceEffortOverride("/ws/a")
	reloaded.Save()
	if got := NewProjectStateStore(path).WorkspaceEffortOverride("/ws/a"); got != "" {
		t.Fatalf("effort after clear = %q, want empty", got)
	}
}

func TestProjectStateStore_SetWorkspaceEffortEmptyClears(t *testing.T) {
	store := NewProjectStateStore("")
	store.SetWorkspaceEffortOverride("/ws/a", "high")
	store.SetWorkspaceEffortOverride("/ws/a", "")
	if got := store.WorkspaceEffortOverride("/ws/a"); got != "" {
		t.Fatalf("an empty effort should clear the entry, got %q", got)
	}
}

// Regression: a model and thinking level chosen for a workspace must outlive
// that workspace's agent. The pool reaps idle workspaces every 15 minutes and
// the whole pool is empty after a restart; before this the rebuilt agent
// silently fell back to the project-level config default.
func TestGetOrCreateWorkspaceAgent_RestoresPersistedModelAndEffort(t *testing.T) {
	e, store, created, mu := newWorkspaceModelEngine(t, "ws-model-restore-agent")

	wsDir := normalizeWorkspacePath(t.TempDir())
	store.SetWorkspaceModelOverride(wsDir, "opus")
	store.SetWorkspaceEffortOverride(wsDir, "max")
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

// The project-level agent's effort is already covered by config.toml, so a
// non-workspace switch must not write a per-workspace entry.
func TestPersistWorkspaceEffortOverride_SkipsProjectLevelAgent(t *testing.T) {
	e, store, _, _ := newWorkspaceModelEngine(t, "ws-effort-skip-agent")

	e.persistWorkspaceEffortOverride("", "plain:c:u", e.agent, "max")
	e.persistWorkspaceEffortOverride("/ws/a:plain:c:u", "plain:c:u", e.agent, "max")

	if got := store.WorkspaceEffortOverride("/ws/a"); got != "" {
		t.Fatalf("project-level agent wrote a per-workspace effort %q", got)
	}
}
