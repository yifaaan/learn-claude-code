package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var validStatuses = map[string]bool{
	"pending":     true,
	"in_progress": true,
	"completed":   true,
}

type Task struct {
	ID          int    `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	BlockedBy   []int  `json:"blocked_by"` // 依赖的任务ID列表
	Blocks      []int  `json:"blocks"`     // 被哪些任务依赖的ID列表
	Status      string `json:"status"`     // "pending", "in_progress", "completed"
	Owner       string `json:"owner"`      // 任务负责人agent的名字
}

type TaskManager struct {
	dir    string // task_*.json文件所在目录
	nextID int    // 下一个可用任务ID
}

func NewTaskManager(dir string) *TaskManager {
	os.MkdirAll(dir, 0o755)
	taskPaths, _ := filepath.Glob(filepath.Join(dir, "task_*.json"))
	maxID := 0
	for _, path := range taskPaths {
		var id int
		fmt.Sscanf(filepath.Base(path), "task_%d.json", &id)
		if id > maxID {
			maxID = id
		}
	}
	return &TaskManager{
		dir:    dir,
		nextID: maxID + 1,
	}
}

func (tm *TaskManager) save(task *Task) error {
	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", task.ID))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(task)
}

func (tm *TaskManager) load(taskID int) (*Task, error) {
	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", taskID))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var task Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (tm *TaskManager) Create(subject, description string) (*Task, error) {
	task := &Task{
		ID:          tm.nextID,
		Subject:     subject,
		Description: description,
		BlockedBy:   []int{},
		Blocks:      []int{},
		Status:      "pending",
		Owner:       "",
	}
	if err := tm.save(task); err != nil {
		return nil, err
	}
	tm.nextID++
	return task, nil
}

func (tm *TaskManager) Get(taskID int) (*Task, error) {
	return tm.load(taskID)
}

// UpdateParams包含了要更新的状态，以及要添加的依赖关系（blocked_by和blocks）。被更新的任务会根据这些参数进行相应的修改。
type UpdateParams struct {
	Status       string
	AddBlockedBy []int
	AddBlocks    []int
}

func (tm *TaskManager) Update(taskID int, params UpdateParams) (*Task, error) {
	task, err := tm.load(taskID)
	if err != nil {
		return nil, err
	}
	if params.Status != "" {
		if !validStatuses[params.Status] {
			return nil, fmt.Errorf("invalid status: %s", params.Status)
		}
		task.Status = params.Status
	}
	if params.AddBlockedBy != nil {
		task.BlockedBy = mergeUnique(task.BlockedBy, params.AddBlockedBy)
	}
	if params.AddBlocks != nil {
		task.Blocks = mergeUnique(task.Blocks, params.AddBlocks)
		// 同时更新被依赖任务的Blocks列表
		for _, blockedID := range params.AddBlocks {
			blocked, err := tm.load(blockedID)
			if err != nil {
				continue
			}

			blocked.BlockedBy = mergeUnique(blocked.BlockedBy, []int{taskID})
			tm.save(blocked)
		}
	}
	if err := tm.save(task); err != nil {
		return nil, err
	}
	return task, nil
}

func (tm *TaskManager) ListAll() string {
	taskPaths, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	slices.Sort(taskPaths)
	lines := make([]string, 0, len(taskPaths))
	for _, path := range taskPaths {
		var id int
		fmt.Sscanf(filepath.Base(path), "task_%d.json", &id)
		task, err := tm.load(id)
		if err != nil {
			continue
		}
		line := fmt.Sprintf("%s #%d: %s %s(blocked by: %v) (blocks: %v) (owner: %s)", statusMarker(task.Status), task.ID, task.Subject, task.Description, task.BlockedBy, task.Blocks, task.Owner)
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "No tasks."
	}
	return strings.Join(lines, "\n")
}

func statusMarker(status string) string {
	switch status {
	case "pending":
		return "[ ]"
	case "in_progress":
		return "[>]"
	case "completed":
		return "[x]"
	default:
		return "[?]"
	}
}

func mergeUnique(a, b []int) []int {
	seen := make(map[int]bool)
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		seen[v] = true
	}
	result := make([]int, 0, len(seen))
	for v := range seen {
		result = append(result, v)
	}
	return result
}
