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
	msgTypePlanApprovalRequest  = "plan_approval_request"
	msgTypePlanApprovalResponse = "plan_approval_response"

	teammateStatusIdle     = "idle"
	teammateStatusWorking  = "working"
	teammateStatusShutdown = "shutdown"

	statusPending  = "pending"
	statusApproved = "approved"
	statusRejected = "rejected"
)

var validMessageTypes = map[string]bool{
	msgTypeMessage:              true,
	msgTypeBroadcast:            true,
	msgTypeShutdownRequest:      true,
	msgTypeShutdownResponse:     true,
	msgTypePlanApprovalRequest:  true,
	msgTypePlanApprovalResponse: true,
}

type TeamMessage struct {
	Type      string         `json:"type"`
	From      string         `json:"from"`
	Content   string         `json:"content"`
	Timestamp int64          `json:"timestamp"`
	Extra     map[string]any `json:"extra,omitempty"`
}

type ProtocolRequest struct {
	RequestID string `json:"request_id"`
	From      string `json:"from"`
	Status    string `json:"status"`
	Payload   string `json:"payload,omitempty"`
}

var (
	shutdownRequests = make(map[string]*ProtocolRequest)
	planRequests     = make(map[string]*ProtocolRequest)
	protocolMu       sync.Mutex
)

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

	if err := os.MkdirAll(b.inboxDir, 0o755); err != nil {
		return fmt.Sprintf("create inbox dir: %v", err)
	}

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

	if err := os.MkdirAll(b.inboxDir, 0o755); err != nil {
		return nil, err
	}

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

		b.Send(from, name, msgTypeBroadcast, content, nil)
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
	nextStatus map[string]string
	mu         sync.Mutex
}

func NewTeammateManager(teamDir string, bus *MessageBus) *TeammateManager {
	_ = os.MkdirAll(teamDir, 0o755)

	tm := &TeammateManager{
		teamDir:    teamDir,
		configPath: filepath.Join(teamDir, "config.json"),
		bus:        bus,
		running:    make(map[string]bool),
		nextStatus: make(map[string]string),
	}

	tm.config = tm.loadConfig()
	tm.resetStaleMembers()
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

func (tm *TeammateManager) resetStaleMembers() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	changed := false
	for i := range tm.config.Members {
		if tm.config.Members[i].Status == teammateStatusWorking {
			tm.config.Members[i].Status = teammateStatusIdle
			changed = true
		}
	}

	if changed {
		_ = tm.saveConfig()
	}
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

func (tm *TeammateManager) IsRunning(name string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.running[strings.TrimSpace(name)]
}

func (tm *TeammateManager) Wake(cfg config, name string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" || name == "lead" {
		return nil
	}

	member := tm.findMember(name)
	if member == nil {
		return fmt.Errorf("teammate not found: %s", name)
	}
	if member.Status == teammateStatusShutdown || tm.running[name] {
		return nil
	}

	member.Status = teammateStatusWorking
	tm.running[name] = true
	role := member.Role

	if err := tm.saveConfig(); err != nil {
		tm.running[name] = false
		member.Status = teammateStatusIdle
		return err
	}

	go tm.teammateLoop(cfg, name, role, "")
	return nil
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
		_ = tm.finishRun(name, tm.consumeNextStatus(name, teammateStatusIdle))
	}()

	system := tm.teammateSystemPrompt(name, role)
	history := make([]apiMessage, 0, 4)
	if strings.TrimSpace(prompt) != "" {
		history = append(history, apiMessage{
			Role:    "user",
			Content: prompt,
		})
	}

	if plan := buildInitialPlanRequest(role, prompt); plan != "" {
		reqID := generateRequestID()
		trackPlanRequest(reqID, name, plan)
		_ = tm.bus.Send(name, "lead", msgTypePlanApprovalRequest, plan, map[string]any{
			"request_id": reqID,
		})
		history = append(history, apiMessage{
			Role:    "user",
			Content: fmt.Sprintf("You already submitted plan approval request %s to the lead. Wait for plan_approval_response before doing any risky implementation.", reqID),
		})
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
			break
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

func buildInitialPlanRequest(role, prompt string) string {
	if !requiresPlanApproval(role, prompt) {
		return ""
	}

	task := strings.TrimSpace(prompt)
	if task == "" {
		task = fmt.Sprintf("Role-based task for %s", strings.TrimSpace(role))
	}

	return fmt.Sprintf(
		"Proposed plan for approval:\n1. Inspect the current implementation and identify the smallest safe refactor surface.\n2. Make the minimum code changes needed for %s.\n3. Run formatting and verification before reporting completion.\nI will wait for approval before editing files.",
		task,
	)
}

func requiresPlanApproval(role, prompt string) bool {
	text := strings.ToLower(strings.TrimSpace(role + " " + prompt))
	if text == "" {
		return false
	}

	keywords := []string{
		"risky",
		"dangerous",
		"destructive",
		"refactor",
		"rewrite",
		"migration",
		"rename",
		"delete",
		"overhaul",
	}
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}

	return false
}

func (tm *TeammateManager) teammateSystemPrompt(name, role string) string {
	return fmt.Sprintf(
		"You are teammate '%s', role: %s, working at %s. "+
			"Use send_message to communicate with the lead or other teammates. "+
			"Use read_inbox when you need to inspect unread messages. "+
			"If a task is risky, destructive, or a broad refactor, submit plan_approval and wait for plan_approval_response before changing files. "+
			"If the lead sends shutdown_request, respond with shutdown_response before ending your run. "+
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
		msgTypePlanApprovalRequest,
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

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "shutdown_response",
			Description: "Respond to a shutdown request from the lead. Approve to shut down gracefully, reject to keep working.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"request_id": map[string]any{
						"type":        "string",
						"description": "The request_id from the shutdown_request message",
					},
					"approve": map[string]any{
						"type":        "boolean",
						"description": "true to approve shutdown, false to reject",
					},
					"reason": map[string]any{
						"type":        "string",
						"description": "Optional reason for the decision",
					},
				},
				"required": []string{"request_id", "approve"},
			},
		},
	})

	tools = append(tools, toolSpec{
		Type: "function",
		Function: toolFunction{
			Name:        "plan_approval",
			Description: "Submit a plan for lead approval before doing major work.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"plan": map[string]any{
						"type":        "string",
						"description": "The plan text to submit for approval",
					},
				},
				"required": []string{"plan"},
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

		extra := map[string]any(nil)
		switch msgType {
		case msgTypePlanApprovalRequest:
			reqID := generateRequestID()
			trackPlanRequest(reqID, sender, input.Content)
			extra = map[string]any{"request_id": reqID}
		}

		result := tm.bus.Send(sender, input.To, msgType, input.Content, extra)
		if input.To != "lead" {
			if err := tm.Wake(cfg, input.To); err != nil {
				return fmt.Sprintf("%s (wake %s: %v)", result, input.To, err)
			}
		}
		return result

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

	case "shutdown_response":
		var input struct {
			RequestID string `json:"request_id"`
			Approve   bool   `json:"approve"`
			Reason    string `json:"reason,omitempty"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		newStatus := statusRejected
		if input.Approve {
			newStatus = statusApproved
			tm.setNextStatus(sender, teammateStatusShutdown)
		}
		updateShutdownRequest(input.RequestID, newStatus)

		extra := map[string]any{
			"request_id": input.RequestID,
			"approve":    input.Approve,
		}
		reason := input.Reason
		if reason == "" {
			reason = fmt.Sprintf("Shutdown %s", newStatus)
		}

		return tm.bus.Send(sender, "lead", msgTypeShutdownResponse, reason, extra)

	case "plan_approval":
		var input struct {
			Plan string `json:"plan"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err)
		}

		reqID := generateRequestID()
		trackPlanRequest(reqID, sender, input.Plan)

		extra := map[string]any{
			"request_id": reqID,
		}
		sendResult := tm.bus.Send(sender, "lead", msgTypePlanApprovalRequest, input.Plan, extra)
		return fmt.Sprintf("%s (request_id=%s). Wait for a plan_approval_response before proceeding with risky work.", sendResult, reqID)
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
	delete(tm.nextStatus, name)

	return tm.saveConfig()
}

func (tm *TeammateManager) setNextStatus(name, status string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.nextStatus[name] = status
}

func (tm *TeammateManager) consumeNextStatus(name, fallback string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	status, ok := tm.nextStatus[name]
	if !ok || strings.TrimSpace(status) == "" {
		return fallback
	}

	delete(tm.nextStatus, name)
	return status
}

func generateRequestID() string {
	return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
}

func trackShutdownRequest(reqID string, from string) {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	shutdownRequests[reqID] = &ProtocolRequest{
		RequestID: reqID,
		From:      from,
		Status:    statusPending,
	}
}

func updateShutdownRequest(reqID string, newStatus string) bool {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	if req, exists := shutdownRequests[reqID]; exists {
		req.Status = newStatus
		return true
	}
	return false
}

func getShutdownRequest(reqID string) *ProtocolRequest {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	return shutdownRequests[reqID]
}

func trackPlanRequest(reqID string, from string, plan string) {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	planRequests[reqID] = &ProtocolRequest{
		RequestID: reqID,
		From:      from,
		Status:    statusPending,
		Payload:   plan,
	}
}

func getPlanRequest(reqID string) *ProtocolRequest {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	return planRequests[reqID]
}

func updatePlanRequest(reqID string, newStatus string) bool {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	if req, exists := planRequests[reqID]; exists {
		req.Status = newStatus
		return true
	}
	return false
}

func listPendingPlanRequests() []ProtocolRequest {
	protocolMu.Lock()
	defer protocolMu.Unlock()
	result := make([]ProtocolRequest, 0)
	for _, req := range planRequests {
		if req.Status == statusPending {
			result = append(result, *req)
		}
	}
	return result
}

func handleShutdownRequest(cfg config, teammate string) string {
	member, err := teammateManager.GetMember(teammate)
	if err != nil {
		return err.Error()
	}

	reqID := generateRequestID()
	trackShutdownRequest(reqID, teammate)

	extra := map[string]any{
		"request_id": reqID,
	}
	result := teamBus.Send("lead", teammate, msgTypeShutdownRequest, "Please shut down gracefully.", extra)

	if member.Status == teammateStatusIdle && !teammateManager.IsRunning(teammate) {
		updateShutdownRequest(reqID, statusApproved)
		if _, setErr := teammateManager.SetStatus(teammate, teammateStatusShutdown); setErr != nil {
			return fmt.Sprintf("%s (auto-shutdown status update failed: %v)", result, setErr)
		}
		_ = teamBus.Send(teammate, "lead", msgTypeShutdownResponse, "Shutdown approved while idle.", map[string]any{
			"request_id": reqID,
			"approve":    true,
		})
		return fmt.Sprintf("%s (auto-approved while idle)", result)
	}

	if err := teammateManager.Wake(cfg, teammate); err != nil {
		return fmt.Sprintf("%s (wake %s: %v)", result, teammate, err)
	}

	return result
}

func handlePlanReview(cfg config, requestID string, approve bool, feedback string) string {
	req := getPlanRequest(requestID)
	if req == nil {
		return fmt.Sprintf("error: unknown plan request_id %q", requestID)
	}

	newStatus := statusApproved
	if !approve {
		newStatus = statusRejected
	}
	updatePlanRequest(requestID, newStatus)

	extra := map[string]any{
		"request_id": requestID,
		"approve":    approve,
		"feedback":   feedback,
	}
	sendResult := teamBus.Send("lead", req.From, msgTypePlanApprovalResponse, feedback, extra)
	if err := teammateManager.Wake(cfg, req.From); err != nil {
		return fmt.Sprintf("plan %s for %s (%s; wake error: %v)", newStatus, req.From, sendResult, err)
	}

	return fmt.Sprintf("plan %s for %s (%s)", newStatus, req.From, sendResult)
}
