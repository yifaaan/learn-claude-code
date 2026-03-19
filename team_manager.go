package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	msgTypeMessage              = "message"
	msgTypeBroadcast            = "broadcast"
	msgTypeShutdownRequest      = "shutdown_request"
	msgTypeShutdownResponse     = "shutdown_response"
	msgTypePlanApprovalResponse = "plan_approval_response"

	teammateStatusIdle     = "idle"
	teammateStatusWorking  = "working"
	teammateStatusShutdown = "shutdown"
)

var validMessageTypes = map[string]bool{
	msgTypeMessage:              true,
	msgTypeBroadcast:            true,
	msgTypeShutdownRequest:      true,
	msgTypeShutdownResponse:     true,
	msgTypePlanApprovalResponse: true,
}

type TeamMessage struct {
	Type      string         `json:"type"`
	From      string         `json:"from"`
	Content   string         `json:"content"`
	Timestamp int64          `json:"timestamp"`
	Extra     map[string]any `json:"extra,omitempty"`
}

type MessageBus struct {
	inboxDir string // .team/inbox
	mu       sync.Mutex
}

func NewMessageBus(inboxDir string) *MessageBus {
	os.MkdirAll(inboxDir, 0o755)

	return &MessageBus{
		inboxDir: inboxDir,
	}
}

func (b *MessageBus) inboxPath(name string) string {
	return filepath.Join(b.inboxDir, name+".jsonl")
}

func (b *MessageBus) Send(from, to, msgType, content string, extra map[string]any) string {
	if !validMessageTypes[msgType] {
		return fmt.Sprintf("invalid message type: %s", msgType)
	}

	msg := TeamMessage{
		Type:      msgType,
		From:      from,
		Content:   content,
		Timestamp: time.Now().Unix(),
	}
	if len(extra) > 0 {
		msg.Extra = extra
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Sprintf("failed to marshal message: %v", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	fp := b.inboxPath(to)
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Sprintf("open inbox: %v", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Sprintf("write message: %v", err)
	}

	return fmt.Sprintf("sent %s to %s", msgType, to)
}

func (b *MessageBus) ReadInbox(name string) ([]TeamMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	fp := b.inboxPath(name)
	data, err := os.ReadFile(fp)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil, nil
	}

	lines := strings.Split(raw, "\n")
	messages := make([]TeamMessage, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var msg TeamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return nil, fmt.Errorf("parse inbox line: %w", err)
		}
		messages = append(messages, msg)
	}

	if err := os.WriteFile(fp, []byte(""), 0o644); err != nil {
		return nil, err
	}
	return messages, nil
}

func (b *MessageBus) Broadcast(from, content string, teammates []string) string {
	count := 0
	for _, name := range teammates {
		if name == from {
			continue
		}

		b.Send(from, name, content, msgTypeBroadcast, nil)
		count++
	}
	return fmt.Sprintf("broadcast to %d teammates", count)
}

type Teammate struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type TeamConfig struct {
	TeamName string     `json:"team_name"`
	Members  []Teammate `json:"members"`
}

type TeammateManager struct {
	teamDir    string
	configPath string
	config     TeamConfig
	bus        *MessageBus
	running    map[string]bool
	mu         sync.Mutex
}

func NewTeammateManager(teamDir string, bus *MessageBus) *TeammateManager {
	_ = os.MkdirAll(teamDir, 0o755)

	tm := &TeammateManager{
		teamDir:    teamDir,
		configPath: filepath.Join(teamDir, "config.json"),
		bus:        bus,
		running:    make(map[string]bool),
	}

	tm.config = tm.loadConfig()
	return tm
}

func (tm *TeammateManager) loadConfig() TeamConfig {
	data, err := os.ReadFile(tm.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return TeamConfig{
				TeamName: "default",
				Members:  []Teammate{},
			}
		}

		return TeamConfig{
			TeamName: "default",
			Members:  []Teammate{},
		}
	}

	var cfg TeamConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return TeamConfig{
			TeamName: "default",
			Members:  []Teammate{},
		}
	}

	if strings.TrimSpace(cfg.TeamName) == "" {
		cfg.TeamName = "default"
	}
	if cfg.Members == nil {
		cfg.Members = []Teammate{}
	}

	return cfg
}

func (tm *TeammateManager) saveConfig() error {
	data, err := json.MarshalIndent(tm.config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(tm.configPath, data, 0o644)
}

func (tm *TeammateManager) findMember(name string) *Teammate {
	for i := range tm.config.Members {
		if tm.config.Members[i].Name == name {
			return &tm.config.Members[i]
		}
	}
	return nil
}

func (tm *TeammateManager) validateSpawnInput(name, role, prompt string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("teammate name is required")
	}
	if strings.TrimSpace(role) == "" {
		return fmt.Errorf("teammate role is required")
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("teammate prompt is required")
	}
	return nil
}

func (tm *TeammateManager) canSpawn(member *Teammate) error {
	if member == nil {
		return fmt.Errorf("member is nil")
	}

	switch member.Status {
	case teammateStatusIdle, teammateStatusShutdown:
		return nil
	case teammateStatusWorking:
		return fmt.Errorf("'%s' is currently %s", member.Name, member.Status)
	default:
		return fmt.Errorf("'%s' has unsupported status %q", member.Name, member.Status)
	}
}

func (tm *TeammateManager) GetMember(name string) (*Teammate, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("teammate name is required")
	}

	member := tm.findMember(name)
	if member == nil {
		return nil, fmt.Errorf("teammate not found: %s", name)
	}
	return member, nil
}

func (tm *TeammateManager) SetStatus(name, status string) (*Teammate, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	name = strings.TrimSpace(name)
	status = strings.TrimSpace(status)

	if name == "" {
		return nil, fmt.Errorf("teammate name is required")
	}

	switch status {
	case teammateStatusIdle, teammateStatusWorking, teammateStatusShutdown:
	default:
		return nil, fmt.Errorf("invalid teammate status: %s", status)
	}

	member := tm.findMember(name)
	if member == nil {
		return nil, fmt.Errorf("teammate not found: %s", name)
	}

	member.Status = status

	if err := tm.saveConfig(); err != nil {
		return nil, err
	}

	return member, nil
}

func (tm *TeammateManager) ListAll() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if len(tm.config.Members) == 0 {
		return "No teammates."
	}

	lines := []string{fmt.Sprintf("Team: %s", tm.config.TeamName)}
	for _, m := range tm.config.Members {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", m.Name, m.Role, m.Status))
	}

	return strings.Join(lines, "\n")
}

func (tm *TeammateManager) MemberNames() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	names := make([]string, 0, len(tm.config.Members))
	for _, m := range tm.config.Members {
		names = append(names, m.Name)
	}
	return names
}

func (tm *TeammateManager) Spawn(cfg config, name, role, prompt string) (*Teammate, error) {
	tm.mu.Lock()

	name = strings.TrimSpace(name)
	role = strings.TrimSpace(role)
	prompt = strings.TrimSpace(prompt)

	if err := tm.validateSpawnInput(name, role, prompt); err != nil {
		tm.mu.Unlock()
		return nil, err
	}

	member := tm.findMember(name)
	if member == nil {
		tm.config.Members = append(tm.config.Members, Teammate{
			Name:   name,
			Role:   role,
			Status: teammateStatusWorking,
		})
		member = &tm.config.Members[len(tm.config.Members)-1]
	} else {
		if err := tm.canSpawn(member); err != nil {
			tm.mu.Unlock()
			return nil, err
		}

		member.Role = role
		member.Status = teammateStatusWorking
	}

	if tm.running[name] {
		tm.mu.Unlock()
		return nil, fmt.Errorf("'%s' is already running", name)
	}

	if err := tm.saveConfig(); err != nil {
		tm.mu.Unlock()
		return nil, err
	}

	tm.running[name] = true

	var result *Teammate
	for i := range tm.config.Members {
		if tm.config.Members[i].Name == name {
			result = &tm.config.Members[i]
			break
		}
	}

	tm.mu.Unlock()

	if result == nil {
		return nil, fmt.Errorf("teammate not found after spawn: %s", name)
	}

	tm.launchTeammate(cfg, name, role, prompt)
	return result, nil
}

func (tm *TeammateManager) launchTeammate(cfg config, name, role, prompt string) {
	go tm.teammateLoop(cfg, name, role, prompt)
}

func (tm *TeammateManager) teammateLoop(cfg config, name, role, prompt string) {
	defer func() {
		_ = tm.finishRun(name, teammateStatusIdle)
	}()

	system := tm.teammateSystemPrompt(name, role)
	history := []apiMessage{
		{
			Role:    "user",
			Content: prompt,
		},
	}

	tools := tm.teammateToolDefinitions()

	for iteration := 0; iteration < 30; iteration++ {
		inboxMessages, err := tm.drainInboxAsMessages(name)
		if err == nil && len(inboxMessages) > 0 {
			history = append(history, inboxMessages...)
		}

		response, err := createChatCompletionWithTools(cfg, system, history, tools)
		if err != nil {
			return
		}

		if len(response.Choices) == 0 {
			return
		}

		message := response.Choices[0].Message
		if len(message.ToolCalls) == 0 {
			reply := strings.TrimSpace(contentText(message.Content))
			if reply == "" {
				reply = "(no summary)"
			}

			history = append(history, apiMessage{
				Role:    "assistant",
				Content: reply,
			})
			return
		}

		history = append(history, apiMessage{
			Role:      "assistant",
			Content:   contentText(message.Content),
			ToolCalls: message.ToolCalls,
		})

		for _, call := range message.ToolCalls {
			output := tm.execTeammateTool(cfg, name, call)
			history = append(history, apiMessage{
				Role:       "tool",
				Content:    output,
				ToolCallID: call.ID,
			})
		}
	}
}

func (tm *TeammateManager) teammateSystemPrompt(name, role string) string {
	return fmt.Sprintf(
		"You are teammate '%s', role: %s, working at %s. "+
			"Use send_message to communicate with the lead or other teammates. "+
			"Use read_inbox when you need to inspect unread messages. "+
			"Complete the assigned task with the available tools.",
		name,
		role,
		mustGetwd(),
	)
}

func validMessageTypeValues() []string {
	return []string{
		msgTypeMessage,
		msgTypeBroadcast,
		msgTypeShutdownRequest,
		msgTypeShutdownResponse,
		msgTypePlanApprovalResponse,
	}
}

func (tm *TeammateManager) teammateToolDefinitions() []toolSpec {
	tools := append([]toolSpec{}, baseToolDefinitions()...)

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "send_message",
			Description: "Send a message to another teammate or to the lead.",
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
			Description: "Read and drain your own inbox.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	})

	return tools
}

func (tm *TeammateManager) execTeammateTool(cfg config, sender string, call toolCall) string {
	if call.Type != "function" {
		return fmt.Sprintf("unsupported tool call type: %s", call.Type)
	}

	switch call.Function.Name {
	case "bash":
		var input bashToolInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		return runBash(input.Command)

	case "read_file":
		var input readFileInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		return runRead(input.Path, input.Limit)

	case "write_file":
		var input writeFileInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		return runWrite(input.Path, input.Content)

	case "edit_file":
		var input editFileInput
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}
		return runEdit(input.Path, input.OldText, input.NewText)

	case "send_message":
		var input struct {
			To      string `json:"to"`
			Content string `json:"content"`
			MsgType string `json:"msg_type,omitempty"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		msgType := strings.TrimSpace(input.MsgType)
		if msgType == "" {
			msgType = msgTypeMessage
		}

		return tm.bus.Send(sender, input.To, msgType, input.Content, nil)

	case "read_inbox":
		msgs, err := tm.bus.ReadInbox(sender)
		if err != nil {
			return fmt.Sprintf("read inbox: %v", err)
		}

		data, err := json.MarshalIndent(msgs, "", "  ")
		if err != nil {
			return fmt.Sprintf("marshal inbox: %v", err)
		}
		return string(data)

	default:
		return fmt.Sprintf("unsupported teammate tool: %s", call.Function.Name)
	}
}

func (tm *TeammateManager) drainInboxAsMessages(name string) ([]apiMessage, error) {
	msgs, err := tm.bus.ReadInbox(name)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}

	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return nil, err
	}

	return []apiMessage{
		{
			Role:    "user",
			Content: fmt.Sprintf("<inbox>\n%s\n</inbox>", string(data)),
		},
	}, nil
}

func (tm *TeammateManager) finishRun(name string, nextStatus string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	member := tm.findMember(name)
	if member == nil {
		tm.running[name] = false
		return fmt.Errorf("teammate not found: %s", name)
	}

	member.Status = nextStatus
	tm.running[name] = false

	return tm.saveConfig()
}
