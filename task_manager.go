package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// 空闲轮询间隔（秒）
	pollInterval = 5 * time.Second
	// 空闲超时时间（秒）- 超过此时间没有新工作就关闭
	idleTimeOut = 60 * time.Second
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
	BlockedBy   []int  `json:"blocked_by"`      // 依赖的任务ID列表
	Blocks      []int  `json:"blocks"`          // 被哪些任务依赖的ID列表
	Status      string `json:"status"`          // "pending", "in_progress", "completed"
	Owner       string `json:"owner"`           // 任务负责人agent的名字
	Worktree    string `json:"worktree,omitempty"` // 绑定的worktree名称
}

type taskCreateInput struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
}

type taskUpdateInput struct {
	ID           int    `json:"id"`
	Status       string `json:"status,omitempty"`
	AddBlockedBy []int  `json:"add_blocked_by,omitempty"`
	AddBlocks    []int  `json:"add_blocks,omitempty"`
}

type taskGetInput struct {
	ID int `json:"id"`
}

type TaskManager struct {
	dir    string // task_*.json文件所在目录
	nextID int    // 下一个可用任务ID
	mu     sync.Mutex
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

// ScanUnclaimedTasks 扫描任务目录，返回所有未被认领且无依赖阻塞的任务
func (tm *TaskManager) ScanUnclaimedTasks() []*Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	taskPaths, err := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	if err != nil {
		return nil
	}

	tasks := make([]*Task, 0)
	for _, path := range taskPaths {
		var id int
		fmt.Sscanf(filepath.Base(path), "task_%d.json", &id)
		task, err := tm.load(id)
		if err != nil {
			continue
		}
		// Agent 需要主动发现可以开始工作的任务。只有 pending 状态、无负责人、无阻塞依赖的任务才能被自动认领
		if task.Status == "pending" && task.Owner == "" && len(task.BlockedBy) == 0 {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

// ClaimTask 认领一个任务，将其分配给指定的 owner
func (tm *TaskManager) ClaimTask(taskID int, owner string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	task, err := tm.load(taskID)
	if err != nil {
		return err
	}
	if task.Status == "pending" && task.Owner == "" && len(task.BlockedBy) == 0 {
		task.Owner = owner
		task.Status = "in_progress"
		tm.saveLocked(task)
		return nil
	}
	return errors.New("task already claimed")
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

func (tm *TaskManager) saveLocked(task *Task) error {
	path := filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", task.ID))
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return nil
	}

	// 先写临时文件
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
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
