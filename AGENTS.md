# Repository Guidelines

## Project Structure & Module Organization
This repository is a small Go CLI agent built as a single `main` package at the repository root. Entry flow starts in `main.go`, then fans out into focused files such as `agent_loop.go` (conversation loop), `tool_use.go` and `sub_agent.go` (tool and agent coordination), `task_manager.go` (task state), `compact.go` (history compaction), and `skill_loader.go` (skill discovery). Reusable skill definitions live under `skills/<skill-name>/SKILL.md`. Local configuration is stored in `.env`; keep it local-only.

## Build, Test, and Development Commands
Use standard Go commands from the repository root:

- `go run .` starts the interactive CLI defined in `main.go`.
- `go test ./...` runs all tests; today it also serves as a compile check because no `*_test.go` files exist yet.
- `go build ./...` verifies the project builds cleanly across packages.
- `gofmt -w *.go` formats all root-level Go files before committing.

## Coding Style & Naming Conventions
Follow default Go formatting with `gofmt`; do not hand-align code. Keep files narrow in purpose and prefer small functions over large orchestration blocks. Use `camelCase` for unexported identifiers and `PascalCase` only when a symbol truly needs to be exported. Since the project is currently a single package, prefer unexported helpers unless a public API is intentional. Name new files after the subsystem they own, for example `memory_store.go` or `retry_policy.go`.

## Testing Guidelines
Add tests beside the code they cover using `*_test.go` files and Go's `testing` package. Prefer table-driven tests for parser, task, and compaction logic. Name tests with `TestXxx` and benchmarks with `BenchmarkXxx`. Before opening a PR, run `go test ./...` and include any manual CLI checks you performed.

## Commit & Pull Request Guidelines
Recent history uses short imperative subjects such as `Add TaskManager implementation...` and `Fix frontmatter parsing...`. Keep commits focused, descriptive, and scoped to one change. Pull requests should include: a short behavior summary, the main files touched, test commands run, and terminal screenshots or sample input/output when CLI behavior changes. Link related issues and call out any `.env` or skill format changes explicitly.

## Security & Configuration Tips
Do not commit `.env`, API keys, or machine-specific paths. When adding new configuration, document the variable name and expected format in the PR description. Keep skill files free of secrets and reproducible on another machine.
