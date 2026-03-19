package main

import (
	"bufio"
	"encoding/json"
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
		fmt.Print("\033[36ms09 >> \033[0m")
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
		if query == "/team" {
			fmt.Println(teammateManager.ListAll())
			fmt.Println()
			continue
		}
		if query == "/inbox" {
			msgs, err := teamBus.ReadInbox("lead")
			if err != nil {
				fmt.Fprintf(os.Stderr, "read inbox: %v\n\n", err)
				continue
			}

			data, err := json.MarshalIndent(msgs, "", "  ")
			if err != nil {
				fmt.Fprintf(os.Stderr, "marshal inbox: %v\n\n", err)
				continue
			}

			fmt.Println(string(data))
			fmt.Println()
			continue
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
