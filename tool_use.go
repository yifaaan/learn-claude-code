package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func safePath(p string) (string, error) {
	wd := mustGetwd()
	wd, err := filepath.Abs(wd)
	if err != nil {
		return "", err
	}

	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("path must be local: %s", p)
	}
	p = filepath.Join(wd, p)
	p = filepath.Clean(p)
	return p, nil
}

type readFileInput struct {
	Path  string `json:"path"`
	Limit *int   `json:"limit,omitempty"`
}

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editFileInput struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

func runRead(path string, limit *int) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("invalid path: %v", err)
	}

	data, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("read file: %v", err)
	}

	text := string(data)
	lines := strings.Split(text, "\n")

	if limit != nil && *limit < len(lines) {
		remains := len(lines) - *limit
		lines = append(lines[:*limit], fmt.Sprintf("... (%d more lines)", remains))
	}
	return truncateText(strings.Join(lines, "\n"), maxToolOutputRunes)
}

func runWrite(path, content string) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("invalid path: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return fmt.Sprintf("create directories: %v", err)
	}

	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("write file: %v", err)
	}

	return fmt.Sprintf("write %d bytes to %s", len(content), path)
}

func runEdit(path, oldText, newText string) string {
	fp, err := safePath(path)
	if err != nil {
		return fmt.Sprintf("invalid path: %v", err)
	}

	data, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("read file: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, oldText) {
		return fmt.Sprintf("old text:%q not found in file", oldText)
	}

	updated := strings.Replace(content, oldText, newText, 1)
	if err := os.WriteFile(fp, []byte(updated), 0o644); err != nil {
		return fmt.Sprintf("write file: %v", err)
	}
	return fmt.Sprintf("replaced first occurrence of old text with new text in %s", path)
}
