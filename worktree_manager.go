package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WorktreeMeta stores metadata for a worktree entry.
type WorktreeMeta struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	TaskID    *int   `json:"task_id,omitempty"`
	Status    string `json:"status"` // "active", "removed", "kept"
	CreatedAt int64  `json:"created_at"`
}

// WorktreeEvent records lifecycle events for worktrees.
type WorktreeEvent struct {
	Event     string        `json:"event"`
	Worktree  *WorktreeMeta `json:"worktree,omitempty"`
	Task      *Task         `json:"task,omitempty"`
	Timestamp int64         `json:"ts"`
}

// WorktreeManager manages git worktrees for task isolation.
type WorktreeManager struct {
	dir        string // .worktrees directory
	tasks      *TaskManager
	mu         sync.Mutex
	eventsPath string // events.jsonl path
}

// NewWorktreeManager creates a new worktree manager.
func NewWorktreeManager(dir string, tasks *TaskManager) *WorktreeManager {
	os.MkdirAll(dir, 0o755)
	return &WorktreeManager{
		dir:        dir,
		tasks:      tasks,
		eventsPath: filepath.Join(dir, "events.jsonl"),
	}
}

// Create creates a new worktree with the given name and optionally binds it to a task.
func (wm *WorktreeManager) Create(name string, taskID *int) (*WorktreeMeta, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	// Check if worktree already exists
	index := wm.loadIndex()
	for _, wt := range index {
		if wt.Name == name {
			return nil, fmt.Errorf("worktree '%s' already exists", name)
		}
	}

	// Create branch name
	branch := "wt/" + name
	wtPath := filepath.Join(wm.dir, name)

	// Emit before event
	wm.emitEvent("worktree.create.before", &WorktreeMeta{Name: name, Branch: branch, TaskID: taskID}, nil)

	// Run git worktree add command
	cmd := exec.Command("git", "worktree", "add", "-b", branch, wtPath, "HEAD")
	cmd.Dir = mustGetwd()
	output, err := cmd.CombinedOutput()
	if err != nil {
		wm.emitEvent("worktree.create.failed", &WorktreeMeta{Name: name, Branch: branch, TaskID: taskID}, nil)
		return nil, fmt.Errorf("git worktree add failed: %v\n%s", err, string(output))
	}

	// Create metadata
	meta := &WorktreeMeta{
		Name:      name,
		Path:      wtPath,
		Branch:    branch,
		TaskID:    taskID,
		Status:    "active",
		CreatedAt: time.Now().Unix(),
	}

	// Update index
	index = append(index, meta)
	if err := wm.saveIndex(index); err != nil {
		// Rollback worktree creation
		exec.Command("git", "worktree", "remove", wtPath).Run()
		return nil, fmt.Errorf("failed to save index: %v", err)
	}

	// Bind to task if taskID is provided
	if taskID != nil {
		if err := wm.bindTaskLocked(*taskID, name); err != nil {
			// Log warning but don't fail
			fmt.Printf("warning: failed to bind task %d: %v\n", *taskID, err)
		}
	}

	// Emit after event
	wm.emitEvent("worktree.create.after", meta, nil)

	return meta, nil
}

// bindTaskLocked binds a task to a worktree. Must be called with lock held.
func (wm *WorktreeManager) bindTaskLocked(taskID int, worktreeName string) error {
	task, err := wm.tasks.Get(taskID)
	if err != nil {
		return err
	}

	// Update task with worktree binding and set status to in_progress if pending
	if task.Status == "pending" {
		task.Status = "in_progress"
	}
	task.Worktree = worktreeName

	// Save the task
	if _, err := wm.tasks.Update(taskID, UpdateParams{Status: task.Status}); err != nil {
		return err
	}

	// Also update worktree field directly
	taskData, err := json.Marshal(task)
	if err != nil {
		return err
	}
	taskPath := filepath.Join(wm.tasks.dir, fmt.Sprintf("task_%d.json", taskID))
	if err := os.WriteFile(taskPath, taskData, 0o644); err != nil {
		return err
	}

	return nil
}

// Remove removes a worktree and optionally completes the bound task.
func (wm *WorktreeManager) Remove(name string, force bool, completeTask bool) (*WorktreeMeta, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	index := wm.loadIndex()
	var meta *WorktreeMeta
	var foundIdx int = -1

	for i, wt := range index {
		if wt.Name == name {
			meta = wt
			foundIdx = i
			break
		}
	}

	if meta == nil {
		return nil, fmt.Errorf("worktree '%s' not found", name)
	}

	// Emit before event
	wm.emitEvent("worktree.remove.before", meta, nil)

	// Run git worktree remove
	args := []string{"worktree", "remove", meta.Path}
	if force {
		args = append(args, "--force")
	}

	cmd := exec.Command("git", args...)
	cmd.Dir = mustGetwd()
	output, err := cmd.CombinedOutput()
	if err != nil {
		wm.emitEvent("worktree.remove.failed", meta, nil)
		return nil, fmt.Errorf("git worktree remove failed: %v\n%s", err, string(output))
	}

	// Update status
	meta.Status = "removed"

	// Remove from index
	index = append(index[:foundIdx], index[foundIdx+1:]...)
	if err := wm.saveIndex(index); err != nil {
		return nil, fmt.Errorf("failed to save index: %v", err)
	}

	// Complete task if requested and bound
	if completeTask && meta.TaskID != nil {
		task, err := wm.tasks.Get(*meta.TaskID)
		if err == nil {
			wm.tasks.Update(*meta.TaskID, UpdateParams{Status: "completed"})
			wm.unbindTask(*meta.TaskID)
			wm.emitEvent("task.completed", meta, task)
		}
	}

	// Emit after event
	wm.emitEvent("worktree.remove.after", meta, nil)

	return meta, nil
}

// Keep marks a worktree as kept (preserved for later use).
func (wm *WorktreeManager) Keep(name string) (*WorktreeMeta, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	index := wm.loadIndex()
	var meta *WorktreeMeta

	for _, wt := range index {
		if wt.Name == name {
			meta = wt
			break
		}
	}

	if meta == nil {
		return nil, fmt.Errorf("worktree '%s' not found", name)
	}

	meta.Status = "kept"
	if err := wm.saveIndex(index); err != nil {
		return nil, fmt.Errorf("failed to save index: %v", err)
	}

	// Emit keep event
	wm.emitEvent("worktree.keep", meta, nil)

	return meta, nil
}

// Run executes a command in the worktree directory.
func (wm *WorktreeManager) Run(name, command string) (string, error) {
	wm.mu.Lock()

	index := wm.loadIndex()
	var meta *WorktreeMeta

	for _, wt := range index {
		if wt.Name == name {
			meta = wt
			break
		}
	}

	wm.mu.Unlock()

	if meta == nil {
		return "", fmt.Errorf("worktree '%s' not found", name)
	}

	if meta.Status != "active" {
		return "", fmt.Errorf("worktree '%s' is not active (status=%s)", name, meta.Status)
	}

	// Run command in worktree directory
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = meta.Path
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("command failed: %v\n%s", err, string(output))
	}

	return string(output), nil
}

// Status returns the status of a specific worktree.
func (wm *WorktreeManager) Status(name string) (*WorktreeMeta, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	index := wm.loadIndex()
	for _, wt := range index {
		if wt.Name == name {
			return wt, nil
		}
	}

	return nil, fmt.Errorf("worktree '%s' not found", name)
}

// ListAll returns a formatted string of all worktrees.
func (wm *WorktreeManager) ListAll() string {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	index := wm.loadIndex()
	if len(index) == 0 {
		return "No worktrees."
	}

	lines := make([]string, 0, len(index))
	for _, wt := range index {
		taskStr := ""
		if wt.TaskID != nil {
			taskStr = fmt.Sprintf(" (task: %d)", *wt.TaskID)
		}
		lines = append(lines, fmt.Sprintf("- %s: branch=%s status=%s%s", wt.Name, wt.Branch, wt.Status, taskStr))
	}

	return strings.Join(lines, "\n")
}

// GetWorktreeByTask finds a worktree bound to a task.
func (wm *WorktreeManager) GetWorktreeByTask(taskID int) *WorktreeMeta {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	index := wm.loadIndex()
	for _, wt := range index {
		if wt.TaskID != nil && *wt.TaskID == taskID {
			return wt
		}
	}
	return nil
}

// loadIndex reads the index.json file.
func (wm *WorktreeManager) loadIndex() []*WorktreeMeta {
	indexPath := filepath.Join(wm.dir, "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return []*WorktreeMeta{}
	}

	var index []*WorktreeMeta
	if err := json.Unmarshal(data, &index); err != nil {
		return []*WorktreeMeta{}
	}

	return index
}

// saveIndex writes the index.json file.
func (wm *WorktreeManager) saveIndex(index []*WorktreeMeta) error {
	indexPath := filepath.Join(wm.dir, "index.json")
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}

	tmp := indexPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}

	return os.Rename(tmp, indexPath)
}

// unbindTask removes worktree binding from a task.
func (wm *WorktreeManager) unbindTask(taskID int) error {
	task, err := wm.tasks.Get(taskID)
	if err != nil {
		return err
	}

	task.Worktree = ""
	taskData, err := json.Marshal(task)
	if err != nil {
		return err
	}

	taskPath := filepath.Join(wm.tasks.dir, fmt.Sprintf("task_%d.json", taskID))
	return os.WriteFile(taskPath, taskData, 0o644)
}

// emitEvent writes an event to events.jsonl.
func (wm *WorktreeManager) emitEvent(eventName string, wt *WorktreeMeta, task *Task) {
	event := WorktreeEvent{
		Event:     eventName,
		Worktree:  wt,
		Task:      task,
		Timestamp: time.Now().Unix(),
	}

	f, err := os.OpenFile(wm.eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.Encode(event)
}

// ListEvents returns recent events from events.jsonl.
func (wm *WorktreeManager) ListEvents(limit int) []WorktreeEvent {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	data, err := os.ReadFile(wm.eventsPath)
	if err != nil {
		return nil
	}

	var events []WorktreeEvent
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var event WorktreeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		events = append(events, event)
	}

	// Return last N events
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}

	return events
}