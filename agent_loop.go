package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
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

// 全局任务状态实例
var todoState = &todoManager{}

// bashToolInput 是传递给 bash 工具的输入参数结构
type bashToolInput struct {
	Command string `json:"command"`
}

type config struct {
	BaseURL string
	APIKey  string
	Model   string
	System  string
	Client  *http.Client
}

// apiMessage 表示一条对话消息。
// 这是整个对话历史 history 的基本单元。
//
// 它需要同时支持几种角色：
// 1. system
// 2. user
// 3. assistant
// 4. tool
//
// 不同角色用到的字段不完全一样，所以这里用一个“通用结构”承载
type apiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`

	// Toolcalls 只在 assistant 请求调用工具时返回，包含工具调用的详细信息
	// 普通的 user/system/tool 消息不包含 ToolCalls 字段
	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	// ToolCallID 只在 role=tool 的消息中使用
	// 当模型请求调用工具时，生成一个唯一的 ToolCallID，后续工具执行结果会通过这个 ID 关联回对应的消息
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// toolCall 表示模型返回的一次工具调用请求
//
//	{
//	  "id": "call_xxx",
//	  "type": "function",
//	  "function": {
//	    "name": "bash",
//	    "arguments": "{\"command\": \"ls -la\"}"
//	  }
//	}
type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

// toolFunction 这个结构会被复用在两个场景：
//
// 场景 1：定义工具时
// - 需要 Name / Description / Parameters
//
// 场景 2：模型返回函数调用时
// - 需要 Name / Arguments
//
// 所以把它们合并进同一个结构里
type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Arguments   string         `json:"arguments,omitempty"` // 序列化的 JSON 字符串(因为参数各不相同），包含模型传入的参数值
}

// toolSpec 表示“我向模型声明了哪些工具可以用”。
// OpenAI 兼容接口里，通常外层会有一个 type=function 的包装
type toolSpec struct {
	Type     string       `json:"type"` // 固定为 "function"
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

// toolDefinitions 返回我声明给模型的工具列表
// 把工具的名字、描述、参数 schema
// 告诉模型，让模型知道自己可以发起什么样的 tool call
func toolDefinitions() []toolSpec {
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
		{
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
		},
	}
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

	return config{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,

		System: fmt.Sprintf("You are a coding agent at %s. Use bash to solve tasks. Act, don't explain.", wd),
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

// createChatCompletion 负责请求 Qwen 的 /chat/completions 接口。
//
// 它做的事情很固定：
// 1. 先把 system 消息放到最前面
// 2. 再拼接历史消息 history
// 3. 把 model / messages / tools 序列化成 JSON
// 4. 发起 HTTP POST 请求
// 5. 读取并解析响应
// 6. 如果接口报错，返回可读的错误信息
func createChatCompletion(cfg config, history []apiMessage) (chatResponse, error) {
	messages := make([]apiMessage, 0, len(history)+2)
	messages = append(messages, apiMessage{
		Role:    "system",
		Content: cfg.System,
	})
	messages = append(messages, history...)

	reqBody, err := json.Marshal(chatRequest{
		Model:    cfg.Model,
		Messages: messages,
		Tools:    toolDefinitions(),
	})
	if err != nil {
		return chatResponse{}, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequest(
		http.MethodPost,
		cfg.BaseURL+"/chat/completions",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return chatResponse{}, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := cfg.Client.Do(req)
	if err != nil {
		return chatResponse{}, fmt.Errorf("request error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatResponse{}, fmt.Errorf("failed to read response body: %w", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return chatResponse{}, fmt.Errorf("failed to parse response body: %w\nResponse body: %s", err, string(respBody))
	}

	if resp.StatusCode >= http.StatusBadRequest {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return chatResponse{}, fmt.Errorf("API error: %s", parsed.Error.Message)
		}

		// 返回的不是标准 error 结构，就把原始 body 截断后带出来。
		return chatResponse{}, fmt.Errorf(
			"qwen api error: status %d: %s",
			resp.StatusCode,
			truncateText(string(respBody), 1000),
		)
	}
	return parsed, nil
}

// runToolCall 负责执行模型请求的单个工具调用。
//
// 它并不直接“理解业务”，只做三件事：
// 1. 校验工具类型和工具名
// 2. 解析 arguments JSON
// 3. 调用 runBash() 执行命令
func runToolCall(call toolCall) string {
	if call.Type != "function" {
		return fmt.Sprintf("unsupported tool call type: %s", call.Type)
	}

	fmt.Printf("\033[33m> %s\033[0m\n", call.Function.Name)

	// 解析模型传入的参数。对于 function 类型的工具调用，参数是一个 JSON 字符串，包含在 call.Function.Arguments 字段里。
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
	default:
		return fmt.Sprintf("unsupported tool: %s", call.Function.Name)
	}
}

func runBash(command string) string {
	lower := strings.ToLower(command)

	for _, fragment := range dangerouseFraments {
		if strings.Contains(lower, strings.ToLower(fragment)) {
			return fmt.Sprintf("command contains dangerous fragment '%s', refusing to execute", fragment)
		}
	}

	// 超时
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

// mustGetwd 提供一个“获取当前目录”的兜底实现。
// 正常情况下 os.Getwd() 不会失败；如果失败，就退回 "."。
func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// truncateText 按 rune 截断字符串，避免直接按字节截断中文导致乱码。
func truncateText(input string, limit int) string {
	runes := []rune(input)
	if len(runes) <= limit {
		return input
	}
	return string(runes[:limit])
}

// contentText 用来把模型返回的 content 安全地转换成字符串。
//
// 前面把 responseMessage.Content / apiMessage.Content 定义成了 any，
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

func agentLoop(cfg config, history *[]apiMessage) (string, error) {
	for {
		response, err := createChatCompletion(cfg, *history)
		if err != nil {
			return "", err
		}

		if len(response.Choices) == 0 {
			return "", fmt.Errorf("no choices in response")
		}

		message := response.Choices[0].Message

		// 本轮模型回复里没有工具调用了，说明模型认为自己已经完成了任务，可以停止了
		if len(message.ToolCalls) == 0 {
			reply := contentText(message.Content)

			// 把模型的最终回复也加到 history 里
			*history = append(*history, apiMessage{
				Role:    "assistant",
				Content: reply,
			})
			return reply, nil
		}

		// 模型请求了工具调用，先把这条消息加到 history 里（包含工具调用信息）
		*history = append(*history, apiMessage{
			Role:      "assistant",
			Content:   contentText(message.Content),
			ToolCalls: message.ToolCalls,
		})

		// 依次执行模型请求的工具调用，把结果一条条加到 history 里
		for _, call := range message.ToolCalls {
			output := runToolCall(call)
			*history = append(*history, apiMessage{
				Role:       "tool",
				Content:    output,
				ToolCallID: call.ID,
			})
			// fmt.Printf("\033[36m[Tool call '%s' output]:\n%s\n\033[0m\n", call.Function.Name, truncateText(output, maxPreviewRunes))
		}
	}
}
