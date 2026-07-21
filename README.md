# Agent Session Butler

**Cross-platform chat-session manager for your AI coding agents.**

Agent Session Butler auto-discovers the AI coding agents installed on your machine (Kiro, Claude Code, …), groups their chat sessions by working directory, and lets you browse, sort, and clean them up — reclaiming disk space and keeping your session history tidy.

A single static Go binary. Runs on Linux, macOS, and Windows. Ships a terminal CLI now; a local `serve` mode with a browser UI is on the roadmap.

> This is the cross-platform successor to the macOS-only SwiftUI app *SessionSweep*. The portable core (agent discovery, session parsing, grouping, orphan detection, mtime-cached enrichment) carried over; the Apple-specific UI and packaging were dropped.

## Features

- **Auto-discovery** — finds installed agents and scans their on-disk sessions. Agents are peers; add one by implementing the `Agent` interface.
- **Grouped by working directory** — every `cwd` you've used an agent in, sorted by most recent activity, with per-directory session count and size.
- **Orphan detection** — directories that no longer exist (project deleted, sessions linger) are flagged `[missing]` — prime cleanup candidates.
- **Lazy enrichment** — message count and titles are computed on demand (`list -v`), so a bare listing stays instant even with tens of MB of `.jsonl`.
- **Lock-aware delete** — sessions held by a **running** agent are lock-detected (live PID check) and protected from deletion.

### Supported agents

| Agent | Session location |
|-------|------------------|
| Kiro | `~/.kiro/sessions/cli/` |
| Claude Code | `~/.claude/projects/<encoded-cwd>/*.jsonl` |

Agent Session Butler is **read-only except for deletion** — it never modifies session content.

## Install / build

Requires Go 1.24+.

```bash
go build -o asbutler ./cmd/asbutler
```

Cross-compile:

```bash
GOOS=linux   GOARCH=amd64 go build -o dist/asbutler-linux    ./cmd/asbutler
GOOS=darwin  GOARCH=arm64 go build -o dist/asbutler-macos    ./cmd/asbutler
GOOS=windows GOARCH=amd64 go build -o dist/asbutler.exe       ./cmd/asbutler
```

## Usage

```bash
asbutler list                 # every session, grouped by working directory
asbutler list -a claude       # only a matching agent (case-insensitive substring)
asbutler list -o              # only orphaned directories (working dir is gone)
asbutler list -v              # also enrich: message count + resolved title per session
asbutler list -a kiro -o -v   # filters and detail all combine
asbutler rm <id>...           # permanently delete sessions by id (locked ones are refused)
asbutler serve                # local browser UI at http://127.0.0.1:7777
asbutler serve --addr :8080   # bind a different host:port
asbutler help
```

`-a` / `--agent` matches the agent name by case-insensitive substring, so `-a claude` selects "Claude Code" — no need to type the full name. `-o` / `--orphans` keeps only groups whose working directory no longer exists — the prime cleanup candidates. Flags compose, and the summary line reflects the filtered set.

### Browser UI (`serve`)

`asbutler serve` starts a local HTTP server and a self-contained browser UI — a two-pane master-detail view: a sidebar lists every working directory (with agent chips, a rollup, orphan count, and directory search), and selecting one shows its sessions in a sortable table (Title / Agent / Messages / Size / Modified / Session ID). Message counts and titles resolve on demand when a directory is opened. Rows are multi-selectable (Select all) for batch delete, and single or batch deletes go behind a confirmation dialog; sessions held by a running agent are lock-protected. A day/night theme toggle is remembered across visits and defaults to the system preference. The frontend (a small Alpine.js app) and its assets are embedded into the binary via `go:embed`, so it needs no network access and ships as a single file. Same core as the CLI — nothing new touches session parsing or deletion.

## Project layout

```
cmd/asbutler/main.go          CLI entry point (list / rm / serve)
internal/agent/
  agent.go                    Agent interface, Session, streaming jsonl reader
  kiro.go                     KiroAgent (.json + .jsonl bundle)
  claude.go                   ClaudeCodeAgent (.jsonl; cwd read from contents)
  lock_unix.go                live-PID lock check (syscall.Kill)
  lock_windows.go             live-PID lock check (OpenProcess)
internal/store/
  store.go                    concurrent scan, mtime cache, grouping, orphan, delete
  util.go                     path + human-size helpers
internal/server/
  server.go                   HTTP routes + JSON view models over the store
  web/                         embedded UI (index.html + vendored alpine.min.js)
```

Only the process-lock check is platform-specific (build-tag split); everything else is shared.

## License

[MIT](LICENSE)
