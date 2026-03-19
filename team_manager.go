package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	msgTypeMessage              = "message"
	msgTypeBroadcast            = "broadcast"
	msgTypeShutdownRequest      = "shutdown_request"
	msgTypeShutdownResponse     = "shutdown_response"
	msgTypePlanApprovalResponse = "plan_approval_response"
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
