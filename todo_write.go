package main

import (
	"fmt"
	"strconv"
	"strings"
)

// todiItem 单个任务
type todoItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

// todoInput 是工具输入
type todoInput struct {
	Items []todoItem `json:"items"`
}

// todoManager 是本地状态容器
type todoManager struct {
	Items []todoItem
}

// Render 将各个任务状态渲染成字符串，供模型查看
func (m *todoManager) Render() string {
	if len(m.Items) == 0 {
		return "No todos."
	}

	lines := make([]string, 0, len(m.Items)+1)
	done := 0

	for _, item := range m.Items {
		marker := "[ ]"

		switch item.Status {
		case "in_progress":
			marker = "[>]"
		case "completed":
			marker = "[x]"
			done++
		}
		lines = append(lines, fmt.Sprintf("%s #%s: %s", marker, item.ID, item.Text))
	}

	lines = append(lines, fmt.Sprintf("(%d/%d completed)", done, len(m.Items)))
	return strings.Join(lines, "\n")
}

func (m *todoManager) Update(items []todoItem) (string, error) {
	if len(items) > 20 {
		return "", fmt.Errorf("too many items: %d", len(items))
	}

	validate := make([]todoItem, 0, len(items))
	inProgressCount := 0

	for i, item := range items {
		id := strings.TrimSpace(item.ID)
		text := strings.TrimSpace(item.Text)
		status := strings.ToLower(strings.TrimSpace(item.Status))

		if id == "" {
			id = strconv.Itoa(i + 1)
		}
		if text == "" {
			return "", fmt.Errorf("item text cannot be empty (id: %s)", id)
		}

		// status 合法
		switch status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("invalid status for item #%s: %s", id, item.Status)
		}

		if status == "in_progress" {
			inProgressCount++
		}

		validate = append(validate, todoItem{
			ID:     id,
			Text:   text,
			Status: status,
		})
	}

	// 最多一个 in_progress
	if inProgressCount > 1 {
		return "", fmt.Errorf("only one item can be in_progress, but got %d", inProgressCount)
	}

	m.Items = validate
	return m.Render(), nil
}
