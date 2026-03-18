package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

/*
Three-layer compression pipeline so the agent can work forever:
    Every turn:
    +------------------+
    | Tool call result |
    +------------------+
            |
            v
    [Layer 1: micro_compact]        (silent, every turn)
      Replace tool_result content older than last 3
      with "[Previous: used {tool_name}]"
            |
            v
    [Check: tokens > 50000?]
       |               |
       no              yes
       |               |
       v               v
    continue    [Layer 2: auto_compact]
                  Save full transcript to .transcripts/
                  Ask LLM to summarize conversation.
                  Replace all messages with [summary].
                        |
                        v
                [Layer 3: compact tool]
                  Model calls compact -> immediate summarization.
                  Same as auto, triggered manually.
Key insight: "The agent can forget strategically and keep working forever."
*/

const (
	compactThreshold = 50000
	keepRecentTools  = 3
)

type compactInput struct {
	Focus string `json:"focus"`
}

func transcriptDir() string {
	return filepath.Join(mustGetwd(), ".transcripts")
}

func estimateTokens(messages []apiMessage) int {
	data, err := json.Marshal(messages)
	if err != nil {
		return 0
	}
	return len(data) / 4 // rough estimate: 4 chars per token
}

func ensureTranscriptDir() error {
	return os.MkdirAll(transcriptDir(), 0o755)
}

func transcriptPath() string {
	return filepath.Join(transcriptDir(), fmt.Sprintf("transcript_%d.jsonl", time.Now().UnixNano()))
}

func saveTranscript(messages []apiMessage) (string, error) {
	if err := ensureTranscriptDir(); err != nil {
		return "", err
	}

	path := transcriptPath()
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, msg := range messages {
		if err := enc.Encode(msg); err != nil {
			return "", err
		}
	}
	return path, nil
}

// role=tool 消息只保存了：

//   - ToolCallID
//   - Content
//
// 并没有直接保存工具名，工具名需要通过toolCallID在之前的消息中找到对应的工具调用来获取。
func toolNameByCallID(messages []apiMessage, toolCallID string) string {
	if strings.TrimSpace(toolCallID) == "" {
		return "unknown_tool"
	}

	for _, msg := range messages {
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}

		for _, call := range msg.ToolCalls {
			if call.ID == toolCallID {
				name := strings.TrimSpace(call.Function.Name)
				if name == "" {
					return "unknown_tool"
				}
				return name
			}
		}
	}
	return "unknown_tool"
}

func microCompact(messages []apiMessage) {
	toolIndexes := make([]int, 0)

	for i, msg := range messages {
		if msg.Role == "tool" {
			toolIndexes = append(toolIndexes, i)
		}
	}

	// 调用过的工具超过 keepRecentTools 个，才进行 compact
	if len(toolIndexes) <= keepRecentTools {
		return
	}

	for _, idx := range toolIndexes[:len(toolIndexes)-keepRecentTools] {
		msg := &messages[idx]
		content := strings.TrimSpace(contentText(msg.Content))
		if len(content) <= 100 {
			continue
		}
		toolName := toolNameByCallID(messages, msg.ToolCallID)
		// 将旧的工具调用结果替换为简短的占位文本
		msg.Content = fmt.Sprintf("[Previous: used %s]", toolName)
	}
}

func autoCompact(cfg config, messages []apiMessage, focus string) ([]apiMessage, error) {
	transcript, err := saveTranscript(messages)
	if err != nil {
		return nil, err
	}

	conversationJSON, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	conversationText := truncateText(string(conversationJSON), 80000)

	prompt := "Summarize this conversation for continuity. Include: " +
		"1) What was accomplished, 2) Current state, 3) Key decisions made. " +
		"Be concise but preserve critical details."
	focus = strings.TrimSpace(focus)
	if focus != "" {
		prompt += "\nAlso preserve this focus: " + focus
	}
	prompt += "\n\n" + conversationText

	summaryHistory := []apiMessage{
		{
			Role:    "user",
			Content: prompt,
		},
	}
	resp, err := createChatCompletionWithTools(cfg, cfg.System, summaryHistory, nil)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in compact response")
	}
	summary := strings.TrimSpace(contentText(resp.Choices[0].Message.Content))
	if summary == "" {
		summary = "Conversation compressed, but no summary was generated."
	}
	return []apiMessage{
		{
			Role:    "user",
			Content: fmt.Sprintf("[Conversation compressed. Transcript: %s]\n\n%s", transcript, summary),
		},
		{
			Role:    "assistant",
			Content: "Understood. I have the context from the summary. Continuing.",
		},
	}, nil
}
