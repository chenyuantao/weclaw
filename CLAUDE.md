# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

WeClaw is a Go CLI that bridges WeChat messages to AI agents via the iLink API. Users chat on WeChat, and messages are routed to configurable AI backends (Claude, Codex, Cursor, Kimi, Gemini, OpenClaw, etc.) through three protocols: ACP (JSON-RPC 2.0 subprocess), CLI (spawned per message), and HTTP (OpenAI-compatible API).

## Build & Run

```bash
# Development with hot reload (requires air)
make dev

# Build binary
go build -o weclaw .

# Run in foreground (for debugging)
./weclaw start -f

# Run as daemon (default)
./weclaw start

# Other commands
./weclaw login          # Add WeChat account (QR code flow)
./weclaw status         # Check daemon status
./weclaw stop           # Stop daemon
./weclaw send --to ID --text "msg"  # Send message proactively
```

Release builds use `CGO_ENABLED=0` and set version via `-ldflags "-X github.com/fastclaw-ai/weclaw/cmd.Version=$TAG"`.

## Architecture

```
WeChat User → iLink API (long-poll) → messaging.Handler → Agent → Response → iLink → WeChat
```

### Key packages

- **`cmd/`** — Cobra CLI commands. `root.go` maps `weclaw` (default) to `start`. Version set via ldflags.
- **`agent/`** — `Agent` interface (`Chat`, `ResetSession`, `Info`) with three implementations:
  - `ACPAgent` — long-running subprocess, JSON-RPC 2.0 over stdin/stdout, session-based
  - `CLIAgent` — spawns new process per message, parses `stream-json` output
  - `HTTPAgent` — stateless OpenAI-compatible chat completions, in-memory history
- **`config/`** — Config loading from `~/.weclaw/config.json` + env vars. `detect.go` auto-discovers installed agents via `exec.LookPath`.
- **`ilink/`** — WeChat iLink API client: auth (QR login), long-poll message monitor, HTTP client for send/receive.
- **`messaging/`** — Message handler (command routing, agent factory, typing indicators), sender, media upload/download, CDN encryption (AES-128-ECB), markdown-to-plaintext conversion.
- **`api/`** — HTTP REST server (`:18011`) for proactive messaging.

### Message routing

The handler supports slash commands: `/agentname message` routes to a config-driven agent, `/status` `/help` `/new` `/clear` `/admin` are built-in. Messages without a command prefix show help with active sessions.

### Agent lifecycle

Agents are created on-demand via `AgentFactory` and cached in `Handler.agents`. The default agent is pre-started at boot. `Handler.getAgent()` uses double-checked locking (RWMutex) for thread safety.

### Session management

Each agent type manages per-conversation sessions differently:
- ACP: `sessions map[conversationID]sessionID` on long-lived subprocess
- CLI: session ID parsed from `stream-json` output, new process each message
- HTTP: `history map[conversationID][]ChatMessage` in memory

## Configuration

Config file: `~/.weclaw/config.json` (perms `0600`)
Accounts: `~/.weclaw/accounts.json`
Workspace: `~/.weclaw/workspace/`
Logs (daemon): `~/.weclaw/weclaw.log`

Environment overrides: `WECLAW_DEFAULT_AGENT`, `WECLAW_API_ADDR`, `OPENCLAW_GATEWAY_URL`, `OPENCLAW_GATEWAY_TOKEN`.

## Conventions

- Log prefixes: `[handler]`, `[acp]`, `[cli]`, `[api]`, `[sender]`
- Errors wrapped with context: `fmt.Errorf("...: %w", err)`
- Agent files named `{protocol}_agent.go` (e.g., `acp_agent.go`, `cli_agent.go`, `http_agent.go`)
- Thread safety: `sync.Mutex` for subprocess state, `sync.RWMutex` for agent cache, `sync.Map` for context tokens
