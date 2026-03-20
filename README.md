# Go AI Agent

一个用 Go 实现的 AI 编程助手，支持任务管理、团队协作和 Git Worktree 隔离。

## 功能特性

### 核心工具

| 工具 | 描述 |
|------|------|
| `bash` | 执行 shell 命令 |
| `read_file` | 读取文件内容 |
| `write_file` | 写入文件 |
| `edit_file` | 精确替换文件内容 |
| `todo` | 管理待办事项列表 |
| `task` | 生成子代理执行任务 |
| `load_skill` | 加载专业技能文档 |

### 任务管理

| 工具 | 描述 |
|------|------|
| `task_create` | 创建新任务 |
| `task_update` | 更新任务状态或依赖 |
| `task_list` | 列出所有任务 |
| `task_get` | 获取单个任务详情 |

任务支持状态流转：`pending` → `in_progress` → `completed`，以及依赖关系管理。

### Git Worktree 隔离 (s12)

| 工具 | 描述 |
|------|------|
| `worktree_create` | 创建隔离的工作目录，可选绑定任务 |
| `worktree_remove` | 删除工作目录，可选完成任务 |
| `worktree_keep` | 标记保留工作目录 |
| `worktree_run` | 在指定工作目录执行命令 |
| `worktree_list` | 列出所有工作目录 |
| `worktree_status` | 查看工作目录状态 |
| `worktree_events` | 查看生命周期事件日志 |

**工作原理：**

```
控制平面 (.tasks/)          执行平面 (.worktrees/)
+------------------+        +------------------------+
| task_1.json      |<------>| auth-refactor/         |
| status: in_progress       | branch: wt/auth-refactor
| worktree: "auth-refactor" | task_id: 1             |
+------------------+        +------------------------+
```

- 每个任务可绑定独立的 Git Worktree
- 任务状态与工作目录自动同步
- 删除工作目录时可自动完成任务

### 团队协作

| 工具 | 描述 |
|------|------|
| `spawn_teammate` | 创建持久化队友 |
| `list_teammates` | 列出所有队友状态 |
| `send_message` | 发送消息到队友收件箱 |
| `read_inbox` | 读取 lead 收件箱 |
| `broadcast` | 广播消息给所有队友 |
| `shutdown_request` | 请求队友优雅关闭 |
| `list_pending_plans` | 列出待审批计划 |
| `plan_review` | 审批或拒绝计划 |

### 后台任务

| 工具 | 描述 |
|------|------|
| `background_run` | 异步执行命令 |
| `check_background` | 检查后台任务状态 |

## 快速开始

### 编译

```bash
go build -o agent .
```

### 配置

创建 `.env` 文件：

```env
QWEN_API_KEY=your-api-key
QWEN_API_BASE_URL=https://api.qwen.com/v1
QWEN_MODEL=qwen3.5-plus
```

### 运行

```bash
./agent
```

## 使用示例

### 任务管理

```
s09 >> Create tasks for backend auth and frontend login page
s09 >> task_list
s09 >> task_update {"id": 1, "status": "in_progress"}
s09 >> task_update {"id": 2, "add_blocked_by": [1]}
```

### Worktree 隔离

```
s09 >> worktree_create {"name": "auth-refactor", "task_id": 1}
s09 >> worktree_run {"name": "auth-refactor", "command": "ls -la"}
s09 >> worktree_list
s09 >> worktree_remove {"name": "auth-refactor", "complete_task": true}
```

### 团队协作

```
s09 >> spawn_teammate {"name": "bob", "role": "backend", "prompt": "You are a backend developer"}
s09 >> send_message {"to": "bob", "content": "Please implement user authentication"}
s09 >> read_inbox
s09 >> shutdown_request {"teammate": "bob"}
```

## 项目结构

```
.
├── main.go              # 入口，REPL 主循环
├── agent_loop.go        # 核心工具调用循环，工具定义
├── task_manager.go      # 任务管理器
├── worktree_manager.go  # Git Worktree 管理器
├── team_manager.go      # 团队/队友管理器
├── sub_agent.go         # 子代理执行
├── compact.go           # 对话压缩
├── tool_use.go          # 文件操作工具
├── todo_write.go        # 待办事项工具
├── skill_loader.go      # 技能加载器
└── skills/              # 技能文档目录
```

## 数据存储

```
.tasks/
├── task_1.json         # 任务详情
├── task_2.json
└── ...

.worktrees/
├── index.json          # worktree 索引
├── events.jsonl        # 生命周期事件日志
├── auth-refactor/      # 实际的 worktree 目录
└── ui-login/

.team/
├── config.json         # 队友配置
└── inbox/
    ├── lead.jsonl      # lead 收件箱
    ├── bob.jsonl       # bob 收件箱
    └── ...
```

## 架构设计

### 核心循环

```
while stop_reason == "tool_use":
    response = LLM(messages, tools)
    execute tools
    append results
    +----------+      +-------+      +---------+
    |   User   | ---> |  LLM  | ---> |  Tool   |
    |  prompt  |      |       |      | execute |
    +----------+      +---+---+      +----+----+
                          ^               |
                          |   tool_result |
                          +---------------+
```

### 三层压缩

1. **micro_compact** (每轮): 将旧工具输出替换为简短占位符
2. **auto_compact** (token > 50000): 保存完整记录，LLM 生成摘要
3. **compact** (手动): 用户触发压缩

## 依赖

- Go 1.21+
- Git (用于 worktree 功能)

## 许可证

MIT