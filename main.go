package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := loadDotEnv(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "load .env: %v\n", err)
		os.Exit(1)
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	history := make([]apiMessage, 0, 32)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	for {
		fmt.Print("\033[36ms06 >> \033[0m")
		if !scanner.Scan() {
			break
		}

		query := strings.TrimSpace(scanner.Text())
		if query == "" {
			break
		}

		lower := strings.ToLower(query)
		if lower == "q" || lower == "exit" {
			break
		}

		history = append(history, apiMessage{
			Role:    "user",
			Content: query,
		})

		reply, err := agentLoop(cfg, &history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent error: %v\n\n", err)
			continue
		}

		if strings.TrimSpace(reply) != "" {
			fmt.Println(reply)
		}

		fmt.Println()
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "read input: %v\n", err)
		os.Exit(1)
	}
}
