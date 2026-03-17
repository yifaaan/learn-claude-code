package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type taskInput struct {
	Prompt      string `json:"prompt"`
	Description string `json:"description"`
}

func createChatCompletionWithTools(cfg config, system string, history []apiMessage, tools []toolSpec) (chatResponse, error) {
	messages := make([]apiMessage, 0, len(history)+2)
	messages = append(messages, apiMessage{
		Role:    "system",
		Content: system,
	})
	messages = append(messages, history...)

	reqBody, err := json.Marshal(chatRequest{
		Model:    cfg.Model,
		Messages: messages,
		Tools:    tools,
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

func runSubagent(cfg config, prompt string) (string, error) {
	fmt.Println("Starting a subagent")
	history := []apiMessage{
		{Role: "user",
			Content: prompt,
		},
	}

	for iteraton := 0; iteraton < 30; iteraton++ {
		response, err := createChatCompletionWithTools(cfg, cfg.SubagentSystem, history, childToolDefinitions())
		if err != nil {
			return "", fmt.Errorf("failed to create chat completion: %w", err)
		}

		if len(response.Choices) == 0 {
			return "", fmt.Errorf("no choices in subagent response")
		}

		message := response.Choices[0].Message

		if len(message.ToolCalls) == 0 {
			summary := strings.TrimSpace(contentText(message.Content))
			if summary == "" {
				summary = "(no summary)"
			}
			return summary, nil
		}

		history = append(history, apiMessage{
			Role:      "assistant",
			Content:   contentText(message.Content),
			ToolCalls: message.ToolCalls,
		})
		for _, call := range message.ToolCalls {
			output := runToolCall(cfg, call)

			history = append(history, apiMessage{
				Role:       "tool",
				Content:    output,
				ToolCallID: call.ID,
			})
		}
	}
	return "", fmt.Errorf("subagent failed to produce a final answer after 30 iterations")
}
