package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

/*

The entire secret of an AI coding agent in one pattern:
    while stop_reason == "tool_use":
        response = LLM(messages, tools)
        execute tools
        append results
    +----------+      +-------+      +---------+
    |   User   | ---> |  LLM  | ---> |  Tool   |
    |  prompt  |      |       |      | execute |
    +----------+      +---+---+      +----+----+
                          ^               |
                          |   tool_result |
                          +---------------+
                          (loop continues)
This is the core loop: feed tool results back to the model
until the model decides to stop. Production agents layer
policy, hooks, and lifecycle controls on top.

*/

const (
	defaultQwenBaseURL = "https://api.qwen.com/v1"

	toolName = "bash"

	commandTimeout     = 120 * time.Second
	maxToolOutputRunes = 50000
	maxPreviewRunes    = 200
)

var dangerouseFraments = []string{
	"rm -rf /",
	"sudo",
	"shutdown",
	"reboot",
	"> /dev/",
}

// Global todo state used by the todo tool.
var todoState = &todoManager{}

// Persistent task manager backed by files in the local tasks directory.
var taskManager = NewTaskManager(filepath.Join(mustGetwd(), "tasks"))

// Persistent team state backed by .team/config.json and .team/inbox/*.jsonl.
var teamBus = NewMessageBus(filepath.Join(mustGetwd(), ".team", "inbox"))

// Global teammate manager for persistent named teammates.
var teammateManager = NewTeammateManager(filepath.Join(mustGetwd(), ".team"), teamBus)

// Global background task manager for asynchronous command execution.
var backgroundManager = NewBackgroundManager()

// Global worktree manager for task isolation.
var worktreeManager = NewWorktreeManager(filepath.Join(mustGetwd(), ".worktrees"), taskManager)

// BackgroundTask stores the current state of one background command.
type BackgroundTask struct {
	ID      string // Stable task ID used by check_background.
	Command string // Command text associated with the task.
	Status  string // running / completed / failed / error
	Result  string // Final output captured for the task.
}

// BackgroundNotification is a queued event injected before the next model call.
type BackgroundNotification struct {
	ID      string // Stable task ID associated with this notification.
	Status  string
	Command string // Command preview for display in the conversation.
	Result  string // Result preview injected back into the conversation.
}

// BackgroundManager coordinates task state and completion notifications.
type BackgroundManager struct {
	mu sync.Mutex

	nextID int
	tasks  map[string]*BackgroundTask

	notifications []BackgroundNotification // Pending completion events.
}

// NewBackgroundManager creates an in-memory background manager.
func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{
		tasks:         make(map[string]*BackgroundTask),
		notifications: make([]BackgroundNotification, 0, 4),
	}
}

// Start registers a task immediately and runs the command in a goroutine.
func (m *BackgroundManager) Start(command string, runner func(string) (string, string)) string {
	m.mu.Lock()

	m.nextID++
	taskID := fmt.Sprintf("bg-%04d", m.nextID)

	m.tasks[taskID] = &BackgroundTask{
		ID:      taskID,
		Command: command,
		Status:  "running",
		Result:  "",
	}

	m.mu.Unlock()

	go func() {
		status, result := runner(command)

		if strings.TrimSpace(result) == "" {
			result = "(no output)"
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		task := m.tasks[taskID]
		task.Status = status
		task.Result = result

		m.notifications = append(m.notifications, BackgroundNotification{
			ID:      taskID,
			Status:  status,
			Command: truncateText(command, 80),
			Result:  truncateText(result, 500),
		})
	}()

	return taskID
}

// DrainNotifications returns all queued notifications and clears the queue.
func (m *BackgroundManager) DrainNotifications() []BackgroundNotification {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]BackgroundNotification, len(m.notifications))
	copy(out, m.notifications)
	m.notifications = m.notifications[:0] // clear queue
	return out
}

// Check returns one task by ID, or all tasks when taskID is empty.
func (m *BackgroundManager) Check(taskID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if strings.TrimSpace(taskID) != "" {
		task, ok := m.tasks[taskID]
		if !ok {
			return fmt.Sprintf("unknown background task: %s", taskID)
		}

		result := task.Result
		if strings.TrimSpace(result) == "" {
			result = "no output"
		}

		return fmt.Sprintf("[%s] %s\n%s", task.Status, truncateText(task.Command, 60), result)
	}

	if len(m.tasks) == 0 {
		return "no background tasks"
	}

	lines := make([]string, 0, len(m.tasks))
	for id, task := range m.tasks {
		lines = append(lines, fmt.Sprintf("%s: [%s] %s", id, task.Status, truncateText(task.Command, 60)))
	}

	return strings.Join(lines, "\n")
}

// bashToolInput is the JSON payload accepted by the bash tool.
type bashToolInput struct {
	Command string `json:"command"`
}

// backgroundRunInput is the JSON payload accepted by background_run.
type backgroundRunInput struct {
	Command string `json:"command"`
}

// checkBackgroundInput is the JSON payload accepted by check_background.
type checkBackgroundInput struct {
	TaskID string `json:"task_id,omitempty"`
}

type spawnTeammateInput struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Prompt string `json:"prompt"`
}

type sendMessageInput struct {
	To      string `json:"to"`
	Content string `json:"content"`
	MsgType string `json:"msg_type,omitempty"`
}

type config struct {
	BaseURL        string
	APIKey         string
	Model          string
	System         string
	SubagentSystem string
	Skills         *SkillLoader
	Client         *http.Client
}

// apiMessage is the common message format stored in conversation history.
type apiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`

	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	ToolCallID string `json:"tool_call_id,omitempty"`
}

//	{
//	  "id": "call_xxx",
//	  "type": "function",
//	  "function": {
//	    "name": "bash",
//	    "arguments": "{\"command\": \"ls -la\"}"
//	  }
//	}
//
// toolCall describes one tool request returned by the model.
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function toolFunction `json:"function"`
}

// toolFunction is reused in tool declarations and tool call payloads.
type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Arguments   string         `json:"arguments,omitempty"` // Serialized JSON arguments from the model.
}

// toolSpec declares one tool that is available to the model.
type toolSpec struct {
	Type     string       `json:"type"` // always "function"
	Function toolFunction `json:"function"`
}

type chatRequest struct {
	Model    string       `json:"model"`
	Messages []apiMessage `json:"messages"`
	Tools    []toolSpec   `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Error   *apiError    `json:"error,omitempty"`
}

type chatChoice struct {
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"` // "stop" or "tool_use"
}

type responseMessage struct {
	Role      string     `json:"role"`
	Content   any        `json:"content"`
	ToolCalls []toolCall `json:"tool_calls"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// baseToolDefinitions returns the core file and shell tools.
func baseToolDefinitions() []toolSpec {
	return []toolSpec{
		{
			Type: "function",
			Function: toolFunction{
				Name:        "bash",
				Description: "Run a shell command.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"command"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "read_file",
				Description: "Read file contents.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"limit": map[string]any{
							"type": "integer",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "write_file",
				Description: "Write content to file.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"content": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: "function",
			Function: toolFunction{
				Name:        "edit_file",
				Description: "Replace exact text in file.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type": "string",
						},
						"old_text": map[string]any{
							"type": "string",
						},
						"new_text": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"path", "old_text", "new_text"},
				},
			},
		},
	}
}
func childToolDefinitions() []toolSpec {
	tools := append([]toolSpec{}, baseToolDefinitions()...)
	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "load_skill",
			Description: "Load specialized knowledge by name.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Skill name to load",
					},
				},
				"required": []string{"name"},
			},
		},
	})
	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "compact",
			Description: "Trigger manul conversation compression",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"focus": map[string]any{
						"type":        "string",
						"description": "What to preserve in the summary.",
					},
				},
			},
		},
	})
	return tools
}

// base + todo + task
func parentToolDefinitions() []toolSpec {
	tools := append([]toolSpec{}, childToolDefinitions()...)
	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "todo",
			Description: "Update task list. Track progress on multi-step tasks.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"items": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id": map[string]any{
									"type": "string",
								},
								"text": map[string]any{
									"type": "string",
								},
								"status": map[string]any{
									"type": "string",
									"enum": []string{"pending", "in_progress", "completed"},
								},
							},
							"required": []string{"id", "text", "status"},
						},
					},
				},
				"required": []string{"items"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "task",
			Description: "Spawn a subagent with fresh context. It shares the filesystem but not conversation history.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type": "string",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "Short description of the task.",
					},
				},
				"required": []string{"prompt"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "task_create",
			Description: "Create a new task.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subject": map[string]any{
						"type": "string",
					},
					"description": map[string]any{
						"type": "string",
					},
				},
				"required": []string{"subject", "description"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "task_update",
			Description: "Update task status or dependencies.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type": "integer",
					},
					"status": map[string]any{
						"type": "string",
						"enum": []string{"pending", "in_progress", "completed"},
					},
					"add_blocked_by": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "integer",
						},
					},
					"add_blocks": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "integer",
						},
					},
				},
				"required": []string{"id"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "task_list",
			Description: "List all tasks.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	})
	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "task_get",
			Description: "Get a specific task.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type": "integer",
					},
				},
				"required": []string{"id"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "background_run",
			Description: "Run a shell command in the background and rreturn a task ID immediately.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command to execute asynchronously.",
					},
				},
				"required": []string{"command"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "check_background",
			Description: "Check one background task by task_id, or list all background tasks when task_id is omitted.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{
						"type":        "string",
						"description": "Optional background task ID such as bg-0001.",
					},
				},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "spawn_teammate",
			Description: "Spawn or resume a persistent teammate that works in its own goroutine and communicates via inbox messages.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type": "string",
					},
					"role": map[string]any{
						"type": "string",
					},
					"prompt": map[string]any{
						"type": "string",
					},
				},
				"required": []string{"name", "role", "prompt"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "list_teammates",
			Description: "List all teammates and their current status.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "send_message",
			Description: "Send a message from the lead to a teammate inbox.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to": map[string]any{
						"type": "string",
					},
					"content": map[string]any{
						"type": "string",
					},
					"msg_type": map[string]any{
						"type": "string",
						"enum": validMessageTypeValues(),
					},
				},
				"required": []string{"to", "content"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "read_inbox",
			Description: "Read and drain the lead inbox.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "broadcast",
			Description: "Broadcast a message from the lead to all teammates.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type": "string",
					},
				},
				"required": []string{"content"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "shutdown_request",
			Description: "Request a teammate to shut down gracefully. Returns a request_id for tracking.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"teammate": map[string]any{
						"type":        "string",
						"description": "Name of the teammate to shut down",
					},
				},
				"required": []string{"teammate"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "plan_review",
			Description: "Approve or reject a teammate's plan. Use list_pending_plans to see pending requests.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{
						"type":        "string",
						"description": "The request_id from the plan_approval_request",
					},
					"approve": map[string]any{
						"type":        "boolean",
						"description": "true to approve, false to reject",
					},
					"feedback": map[string]any{
						"type":        "string",
						"description": "Optional feedback for the teammate",
					},
				},
				"required": []string{"request_id", "approve"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "list_pending_plans",
			Description: "List all pending plan approval requests from teammates.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	})

	// Worktree tools for task isolation
	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "worktree_create",
			Description: "Create a new git worktree for isolated work. Optionally bind to a task to auto-set status to in_progress.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Worktree name (used for directory and branch)",
					},
					"task_id": map[string]any{
						"type":        "integer",
						"description": "Optional task ID to bind to this worktree",
					},
				},
				"required": []string{"name"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "worktree_remove",
			Description: "Remove a worktree. Set complete_task=true to also mark the bound task as completed.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Worktree name to remove",
					},
					"force": map[string]any{
						"type":        "boolean",
						"description": "Force removal even with uncommitted changes",
					},
					"complete_task": map[string]any{
						"type":        "boolean",
						"description": "Also mark bound task as completed",
					},
				},
				"required": []string{"name"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "worktree_keep",
			Description: "Mark a worktree as kept for later use.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Worktree name to keep",
					},
				},
				"required": []string{"name"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "worktree_run",
			Description: "Run a shell command in a worktree directory.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Worktree name",
					},
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command to run",
					},
				},
				"required": []string{"name", "command"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "worktree_list",
			Description: "List all worktrees and their status.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "worktree_status",
			Description: "Get status of a specific worktree.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Worktree name",
					},
				},
				"required": []string{"name"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "worktree_events",
			Description: "List recent worktree lifecycle events.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of events to return",
					},
				},
			},
		},
	})

	return tools
}

func loadConfig() (config, error) {
	wd, err := os.Getwd()
	if err != nil {
		return config{}, err
	}

	baseURL := strings.TrimSpace(os.Getenv("QWEN_API_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultQwenBaseURL
	}

	apiKey := strings.TrimSpace(os.Getenv("QWEN_API_KEY"))
	if apiKey == "" {
		return config{}, fmt.Errorf("QWEN_API_KEY is required")
	}

	model := strings.TrimSpace(os.Getenv("QWEN_MODEL"))
	if model == "" {
		model = "qwen3.5-plus"
	}

	skillsDir := filepath.Join(wd, "skills")
	skills, err := NewSkillLoader(skillsDir)
	if err != nil {
		return config{}, fmt.Errorf("failed to load skills: %v", err)
	}

	return config{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,

		System: fmt.Sprintf("You are a coding agent at %s. Use the todo tool to plan multi-step tasks. Mark in_progress before starting, completed when done. Prefer tools over prose.\nWhen the user asks for persistent teammates or team communication, prefer spawn_teammate, send_message, read_inbox, list_teammates, and broadcast instead of using task as a workaround.\nUse shutdown_request for graceful teammate shutdowns, and use list_pending_plans plus plan_review when teammates submit risky-work plans for approval.\nUse load_skill to access specialized knowledge before tackling unfamiliar topics.\nSkills available:\n%s",
			wd, skills.Descriptions()),
		SubagentSystem: fmt.Sprintf(
			"You are a coding subagent at %s. Complete the given task, then summarize your findings.\nUse load_skill to access specialized knowledge before tackling unfamiliar topics.\nSkills available:\n%s",
			wd, skills.Descriptions()),
		Skills: skills,
		Client: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}, nil
}

func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	lines := strings.Split(string(data), "\n")

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		value := strings.TrimSpace(v)
		value = strings.Trim(value, `"'`)

		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}

// createChatCompletion calls the chat completion API with the parent tool set.
func createChatCompletion(cfg config, history []apiMessage) (chatResponse, error) {
	return createChatCompletionWithTools(cfg, cfg.System, history, parentToolDefinitions())
}

// runToolCall decodes tool arguments and dispatches to the local implementation.
func runToolCall(cfg config, call toolCall) string {
	if call.Type != "function" {
		return fmt.Sprintf("unsupported tool call type: %s", call.Type)
	}

	fmt.Printf("\033[33m> %s\033[0m\n", call.Function.Name)

	switch call.Function.Name {
	case "bash":
		var input bashToolInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		output := runBash(input.Command)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "read_file":
		var input readFileInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		output := runRead(input.Path, input.Limit)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "write_file":
		var input writeFileInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		output := runWrite(input.Path, input.Content)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "edit_file":
		var input editFileInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		output := runEdit(input.Path, input.OldText, input.NewText)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "todo":
		var input todoInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		output, err := todoState.Update(input.Items)
		if err != nil {
			return fmt.Sprintf("failed to update todo list: %v", err)
		}
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "task":
		var input taskInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		desc := strings.TrimSpace(input.Description)
		if desc == "" {
			desc = "subtask"
		}
		fmt.Printf("> task (%s): %s\n", desc, truncateText(input.Prompt, 80))

		output, err := runSubagent(cfg, input.Prompt)
		if err != nil {
			return fmt.Sprintf("failed to run subagent: %v", err)
		}

		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "load_skill":
		var input struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		if cfg.Skills == nil {
			return "skill loader not configured"
		}

		fmt.Printf("> load_skill: %s\n", strings.TrimSpace(input.Name))
		output := cfg.Skills.Content(strings.TrimSpace(input.Name))
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "task_create":
		var input taskCreateInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		task, err := taskManager.Create(input.Subject, input.Description)
		if err != nil {
			return fmt.Sprintf("failed to create task: %v", err)
		}
		output := fmt.Sprintf("Created task #%d: %s - %s", task.ID, task.Subject, task.Description)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "task_update":
		var input taskUpdateInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		params := UpdateParams{
			Status:       input.Status,
			AddBlockedBy: input.AddBlockedBy,
			AddBlocks:    input.AddBlocks,
		}
		task, err := taskManager.Update(input.ID, params)
		if err != nil {
			return fmt.Sprintf("failed to update task: %v", err)
		}
		output := fmt.Sprintf("Updated task #%d: status=%s, blocked_by=%v, blocks=%v", task.ID, task.Status, task.BlockedBy, task.Blocks)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "task_list":
		output := taskManager.ListAll()
		fmt.Println(output)
		return output

	case "task_get":
		var input struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		task, err := taskManager.Get(input.ID)
		if err != nil {
			return fmt.Sprintf("failed to get task: %v", err)
		}
		output := fmt.Sprintf("Task #%d: %s - %s (status=%s, blocked_by=%v, blocks=%v, owner=%s)", task.ID, task.Subject, task.Description, task.Status, task.BlockedBy, task.Blocks, task.Owner)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "background_run":
		var input backgroundRunInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		taskID := backgroundManager.Start(input.Command, runBackgroundCommand)

		output := fmt.Sprintf("Background task %s started: %s", taskID, truncateText(input.Command, 80))
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "check_background":
		var input checkBackgroundInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		output := backgroundManager.Check(input.TaskID)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "spawn_teammate":
		var input spawnTeammateInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		member, err := teammateManager.Spawn(cfg, input.Name, input.Role, input.Prompt)
		if err != nil {
			return fmt.Sprintf("failed to spawn teammate: %v", err)
		}

		output := fmt.Sprintf("spawned teammate %s (%s): %s", member.Name, member.Role, member.Status)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "list_teammates":
		output := teammateManager.ListAll()
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "send_message":
		var input sendMessageInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		msgType := strings.TrimSpace(input.MsgType)
		if msgType == "" {
			msgType = msgTypeMessage
		}

		output := teamBus.Send("lead", input.To, msgType, input.Content, nil)
		if input.To != "lead" {
			if err := teammateManager.Wake(cfg, input.To); err != nil && !strings.Contains(err.Error(), "teammate not found") {
				output = fmt.Sprintf("%s (wake %s: %v)", output, input.To, err)
			}
		}
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "read_inbox":
		msgs, err := teamBus.ReadInbox("lead")
		if err != nil {
			return fmt.Sprintf("failed to read lead inbox: %v", err)
		}

		data, err := json.MarshalIndent(msgs, "", "  ")
		if err != nil {
			return fmt.Sprintf("failed to marshal lead inbox: %v", err)
		}

		output := string(data)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "broadcast":
		var input struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		output := teamBus.Broadcast("lead", input.Content, teammateManager.MemberNames())
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "shutdown_request":
		var input struct {
			Teammate string `json:"teammate"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		output := handleShutdownRequest(cfg, input.Teammate)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "list_pending_plans":
		pending := listPendingPlanRequests()
		data, err := json.MarshalIndent(pending, "", "  ")
		if err != nil {
			return fmt.Sprintf("failed to marshal pending plans: %v", err)
		}
		output := string(data)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "plan_review":
		var input struct {
			RequestID string `json:"request_id"`
			Approve   bool   `json:"approve"`
			Feedback  string `json:"feedback,omitempty"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		output := handlePlanReview(cfg, input.RequestID, input.Approve, input.Feedback)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	// Worktree tool handlers
	case "worktree_create":
		var input struct {
			Name   string `json:"name"`
			TaskID *int   `json:"task_id,omitempty"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		meta, err := worktreeManager.Create(input.Name, input.TaskID)
		if err != nil {
			return fmt.Sprintf("failed to create worktree: %v", err)
		}

		taskStr := ""
		if meta.TaskID != nil {
			taskStr = fmt.Sprintf(" (task: %d)", *meta.TaskID)
		}
		output := fmt.Sprintf("Created worktree '%s' at %s with branch %s%s", meta.Name, meta.Path, meta.Branch, taskStr)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "worktree_remove":
		var input struct {
			Name         string `json:"name"`
			Force        bool   `json:"force,omitempty"`
			CompleteTask bool   `json:"complete_task,omitempty"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		meta, err := worktreeManager.Remove(input.Name, input.Force, input.CompleteTask)
		if err != nil {
			return fmt.Sprintf("failed to remove worktree: %v", err)
		}

		output := fmt.Sprintf("Removed worktree '%s' (branch: %s)", meta.Name, meta.Branch)
		if input.CompleteTask && meta.TaskID != nil {
			output += fmt.Sprintf(" and completed task %d", *meta.TaskID)
		}
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "worktree_keep":
		var input struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		meta, err := worktreeManager.Keep(input.Name)
		if err != nil {
			return fmt.Sprintf("failed to keep worktree: %v", err)
		}

		output := fmt.Sprintf("Worktree '%s' marked as kept", meta.Name)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "worktree_run":
		var input struct {
			Name    string `json:"name"`
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		result, err := worktreeManager.Run(input.Name, input.Command)
		if err != nil {
			return fmt.Sprintf("failed to run in worktree: %v", err)
		}

		output := fmt.Sprintf("[%s] %s", input.Name, truncateText(result, maxToolOutputRunes))
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "worktree_list":
		output := worktreeManager.ListAll()
		fmt.Println(output)
		return output

	case "worktree_status":
		var input struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		meta, err := worktreeManager.Status(input.Name)
		if err != nil {
			return fmt.Sprintf("failed to get worktree status: %v", err)
		}

		taskStr := ""
		if meta.TaskID != nil {
			taskStr = fmt.Sprintf(", task: %d", *meta.TaskID)
		}
		output := fmt.Sprintf("Worktree '%s': path=%s, branch=%s, status=%s%s, created=%d",
			meta.Name, meta.Path, meta.Branch, meta.Status, taskStr, meta.CreatedAt)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	case "worktree_events":
		var input struct {
			Limit int `json:"limit,omitempty"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		if input.Limit == 0 {
			input.Limit = 10
		}

		events := worktreeManager.ListEvents(input.Limit)
		data, err := json.MarshalIndent(events, "", "  ")
		if err != nil {
			return fmt.Sprintf("failed to marshal events: %v", err)
		}

		output := string(data)
		fmt.Println(truncateText(output, maxPreviewRunes))
		return output

	default:
		return fmt.Sprintf("unsupported tool: %s", call.Function.Name)
	}
}

// runBackgroundCommand executes a command asynchronously and returns status plus output.
func runBackgroundCommand(command string) (status string, result string) {
	lower := strings.ToLower(command)

	for _, fragment := range dangerouseFraments {
		if strings.Contains(lower, strings.ToLower(fragment)) {
			return "error", fmt.Sprintf("command contains dangerous fragment '%s', refusing to execute", fragment)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var cmd *exec.Cmd

	if isWindows() {
		if hasBash() {
			cmd = exec.CommandContext(ctx, "bash", "-c", command)
		} else {
			psCommand := strings.ReplaceAll(command, "&&", ";")
			cmd = exec.CommandContext(ctx, "powershell", "-Command", psCommand)
		}
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}

	cmd.Dir = mustGetwd()

	output, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		return "timed_out", "Error: command timed out"
	}

	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return "error", err.Error()
		}
		return "failed", truncateText(text, maxToolOutputRunes)
	}
	if text == "" {
		text = "(no output)"
	}
	return "completed", truncateText(text, maxToolOutputRunes)
}

// isWindows reports whether the current process is running on Windows.
func isWindows() bool {
	return strings.Contains(strings.ToLower(os.Getenv("OS")), "windows") ||
		strings.HasSuffix(strings.ToLower(os.Getenv("COMSPEC")), ".exe")
}

// hasBash reports whether bash is available in PATH.
func hasBash() bool {
	_, err := exec.LookPath("bash")
	return err == nil
}

// runBash executes a blocking shell command with a timeout.
func runBash(command string) string {
	lower := strings.ToLower(command)

	for _, fragment := range dangerouseFraments {
		if strings.Contains(lower, strings.ToLower(fragment)) {
			return fmt.Sprintf("command contains dangerous fragment '%s', refusing to execute", fragment)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-Command", command)
	cmd.Dir = mustGetwd()

	output, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		return "Error: command timed out"
	}

	text := strings.TrimSpace(string(output))

	if err != nil && text == "" {
		text = err.Error()
	}

	if text == "" {
		text = "(no output)"
	}

	return truncateText(text, maxToolOutputRunes)
}

// mustGetwd returns the current working directory, or "." on failure.
func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// truncateText truncates by rune count to avoid breaking UTF-8 characters.
func truncateText(input string, limit int) string {
	runes := []rune(input)
	if len(runes) <= limit {
		return input
	}
	return string(runes[:limit])
}

// contentText converts API content into a plain string representation.
func contentText(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprintf("failed to marshal content: %v", err)
		}
		return string(data)
	}
}

// agentLoop is the core tool-use loop for the parent agent.
func agentLoop(cfg config, history *[]apiMessage) (string, error) {
	roundsSinceTodo := 0

	for {
		leadInbox, err := teamBus.ReadInbox("lead")
		if err == nil && len(leadInbox) > 0 {
			data, marshalErr := json.MarshalIndent(leadInbox, "", "  ")
			if marshalErr == nil {
				*history = append(*history, apiMessage{
					Role:    "user",
					Content: fmt.Sprintf("<inbox>\n%s\n</inbox>", string(data)),
				})
			}
		}

		notifs := backgroundManager.DrainNotifications()
		if len(notifs) > 0 {
			var lines []string
			for _, n := range notifs {
				lines = append(lines, fmt.Sprintf("[bg:%s] %s:%s", n.ID, n.Status, n.Result))
			}

			*history = append(*history, apiMessage{
				Role:    "user",
				Content: fmt.Sprintf("<background-results>\n%s\n</background-results>", strings.Join(lines, "\n")),
			})
		}

		microCompact(*history)

		if estimateTokens(*history) > compactThreshold {
			fmt.Println("[auto_compact triggered]")
			compacted, err := autoCompact(cfg, *history, "")
			if err != nil {
				return "", err
			}
			*history = compacted
		}
		response, err := createChatCompletion(cfg, *history)
		if err != nil {
			return "", err
		}

		if len(response.Choices) == 0 {
			return "", fmt.Errorf("no choices in response")
		}

		message := response.Choices[0].Message

		if len(message.ToolCalls) == 0 {
			reply := contentText(message.Content)

			*history = append(*history, apiMessage{
				Role:    "assistant",
				Content: reply,
			})
			return reply, nil
		}

		*history = append(*history, apiMessage{
			Role:      "assistant",
			Content:   contentText(message.Content),
			ToolCalls: message.ToolCalls,
		})

		usedTodo := false

		manualCompact := false
		manualCompactFocus := ""

		for _, call := range message.ToolCalls {
			var output string
			if call.Function.Name == "compact" {
				manualCompact = true

				var input compactInput
				if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
					output = fmt.Sprintf("invalid tool arguments: %v", err)
				} else {
					manualCompactFocus = input.Focus
					output = "Compressing..."
				}
				fmt.Println(truncateText(output, maxPreviewRunes))
			} else {
				output = runToolCall(cfg, call)
			}

			*history = append(*history, apiMessage{
				Role:       "tool",
				Content:    output,
				ToolCallID: call.ID,
			})
			if call.Function.Name == "todo" {
				usedTodo = true
			}
			// fmt.Printf("\033[36m[Tool call '%s' output]:\n%s\n\033[0m\n", call.Function.Name, truncateText(output, maxPreviewRunes))
		}

		if manualCompact {
			fmt.Println("[manual compact]")
			compacted, err := autoCompact(cfg, *history, manualCompactFocus)
			if err != nil {
				return "", fmt.Errorf("failed to compact conversation: %v", err)
			}
			*history = compacted
		}

		if usedTodo {
			roundsSinceTodo = 0
		} else {
			roundsSinceTodo++
		}

		if roundsSinceTodo >= 3 {
			*history = append(*history, apiMessage{
				Role:    "user",
				Content: "<reminder>Update your todos.</reminder>",
			})
		}
	}
}
