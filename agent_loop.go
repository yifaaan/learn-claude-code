package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
				Name:        toolName,
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
	message = append(messages, apiMessage{
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
