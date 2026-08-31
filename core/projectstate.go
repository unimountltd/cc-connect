package core

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

type projectStateData struct {
	WorkDirOverride         string            `json:"work_dir_override,omitempty"`
	WorkspaceDirOverrides   map[string]string `json:"workspace_dir_overrides,omitempty"`
	WorkspaceModelOverrides map[string]string `json:"workspace_model_overrides,omitempty"`
	InjectPrompts           map[string]string `json:"inject_prompts,omitempty"` // channelID → custom prompt
	// WorkspaceEffortOverrides mirrors WorkspaceModelOverrides for reasoning
	// effort, which upstream does not persist. /reasoning and /preset write
	// here so a workspace's thinking level survives the idle reap that
	// destroys its agent, and a daemon restart, just like its model does.
	WorkspaceEffortOverrides map[string]string `json:"workspace_effort_overrides,omitempty"`
}

// ProjectStateStore persists lightweight runtime state for one project.
type ProjectStateStore struct {
	mu        sync.RWMutex
	storePath string
	state     projectStateData
}

func NewProjectStateStore(path string) *ProjectStateStore {
	ps := &ProjectStateStore{storePath: path}
	if path != "" {
		ps.load()
	}
	return ps
}

func (ps *ProjectStateStore) WorkDirOverride() string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.state.WorkDirOverride
}

func (ps *ProjectStateStore) SetWorkDirOverride(dir string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.state.WorkDirOverride = dir
}

func (ps *ProjectStateStore) WorkspaceDirOverride(workspace string) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.state.WorkspaceDirOverrides == nil {
		return ""
	}
	return ps.state.WorkspaceDirOverrides[workspace]
}

func (ps *ProjectStateStore) SetWorkspaceDirOverride(workspace, dir string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.WorkspaceDirOverrides == nil {
		ps.state.WorkspaceDirOverrides = make(map[string]string)
	}
	ps.state.WorkspaceDirOverrides[workspace] = dir
}

func (ps *ProjectStateStore) ClearWorkspaceDirOverride(workspace string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.WorkspaceDirOverrides == nil {
		return
	}
	delete(ps.state.WorkspaceDirOverrides, workspace)
	if len(ps.state.WorkspaceDirOverrides) == 0 {
		ps.state.WorkspaceDirOverrides = nil
	}
}

func (ps *ProjectStateStore) WorkspaceModelOverride(workspace string) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.state.WorkspaceModelOverrides == nil {
		return ""
	}
	return ps.state.WorkspaceModelOverrides[workspace]
}

func (ps *ProjectStateStore) SetWorkspaceModelOverride(workspace, model string) {
	if model == "" {
		ps.ClearWorkspaceModelOverride(workspace)
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.WorkspaceModelOverrides == nil {
		ps.state.WorkspaceModelOverrides = make(map[string]string)
	}
	ps.state.WorkspaceModelOverrides[workspace] = model
}

func (ps *ProjectStateStore) ClearWorkspaceModelOverride(workspace string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.WorkspaceModelOverrides == nil {
		return
	}
	delete(ps.state.WorkspaceModelOverrides, workspace)
	if len(ps.state.WorkspaceModelOverrides) == 0 {
		ps.state.WorkspaceModelOverrides = nil
	}
}

func (ps *ProjectStateStore) ClearWorkDirOverride() {
	ps.SetWorkDirOverride("")
}

// WorkspaceEffortOverride returns the persisted reasoning effort for a
// workspace directory, or "" when the project default applies.
func (ps *ProjectStateStore) WorkspaceEffortOverride(workspace string) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.state.WorkspaceEffortOverrides == nil {
		return ""
	}
	return ps.state.WorkspaceEffortOverrides[workspace]
}

// SetWorkspaceEffortOverride records the reasoning effort for a workspace
// directory. An empty effort clears the entry so the project default applies.
func (ps *ProjectStateStore) SetWorkspaceEffortOverride(workspace, effort string) {
	if effort == "" {
		ps.ClearWorkspaceEffortOverride(workspace)
		return
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.WorkspaceEffortOverrides == nil {
		ps.state.WorkspaceEffortOverrides = make(map[string]string)
	}
	ps.state.WorkspaceEffortOverrides[workspace] = effort
}

func (ps *ProjectStateStore) ClearWorkspaceEffortOverride(workspace string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.WorkspaceEffortOverrides == nil {
		return
	}
	delete(ps.state.WorkspaceEffortOverrides, workspace)
	if len(ps.state.WorkspaceEffortOverrides) == 0 {
		ps.state.WorkspaceEffortOverrides = nil
	}
}

func (ps *ProjectStateStore) GetInjectPrompt(channelID string) string {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if ps.state.InjectPrompts == nil {
		return ""
	}
	return ps.state.InjectPrompts[channelID]
}

func (ps *ProjectStateStore) SetInjectPrompt(channelID, prompt string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.InjectPrompts == nil {
		ps.state.InjectPrompts = make(map[string]string)
	}
	ps.state.InjectPrompts[channelID] = prompt
}

func (ps *ProjectStateStore) ClearInjectPrompt(channelID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.state.InjectPrompts == nil {
		return
	}
	delete(ps.state.InjectPrompts, channelID)
	if len(ps.state.InjectPrompts) == 0 {
		ps.state.InjectPrompts = nil
	}
}

func (ps *ProjectStateStore) Save() {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	ps.saveLocked()
}

func (ps *ProjectStateStore) saveLocked() {
	if ps.storePath == "" {
		return
	}

	data, err := json.MarshalIndent(ps.state, "", "  ")
	if err != nil {
		slog.Error("project_state: failed to marshal", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(ps.storePath), 0o755); err != nil {
		slog.Error("project_state: failed to create dir", "path", ps.storePath, "error", err)
		return
	}
	if err := AtomicWriteFile(ps.storePath, data, 0o644); err != nil {
		slog.Error("project_state: failed to write", "path", ps.storePath, "error", err)
	}
}

func (ps *ProjectStateStore) load() {
	data, err := os.ReadFile(ps.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("project_state: failed to read", "path", ps.storePath, "error", err)
		}
		return
	}

	var state projectStateData
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Error("project_state: failed to unmarshal", "path", ps.storePath, "error", err)
		return
	}
	ps.state = state
}
