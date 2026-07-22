# Agent Session Butler

**Cross-platform chat-session manager for your AI coding agents.**

Agent Session Butler auto-discovers the AI coding agents installed on your machine (Kiro, Claude Code, …), groups their chat sessions by working directory, and lets you browse, sort, and clean them up — reclaiming disk space and keeping your session history tidy.

A single static Go binary. Runs on Linux, macOS, and Windows. Provides a terminal CLI and a local `webui` mode with a browser UI.

> This is the cross-platform successor to the macOS-only SwiftUI app *SessionSweep*. The portable core (agent discovery, session parsing, grouping, orphan detection, mtime-cached enrichment) carried over; the Apple-specific UI and packaging were dropped.

## Features

- **Auto-discovery** — finds installed agents and scans their on-disk sessions. Agents are peers; add one by implementing the `Agent` interface.
- **Grouped by working directory** — every `cwd` you've used an agent in, sorted by most recent activity, with per-directory session count and size.
- **Orphan detection** — directories that no longer exist (project deleted, sessions linger) are flagged `[missing]` — prime cleanup candidates.
- **Lazy enrichment** — message count and titles are computed on demand (`list -v`), so a bare listing stays instant even with tens of MB of `.jsonl`.
- **Lock-aware delete** — sessions held by a **running** agent are lock-detected (live PID check) and protected from deletion.

### Supported agents

| Agent | Session storage |
|-------|-----------------|
| Kiro | files under `~/.kiro/sessions/cli/` |
| Claude Code | `~/.claude/projects/<encoded-cwd>/*.jsonl` |
| Hermes | SQLite `state.db` per profile under `$HERMES_HOME` (default `~/.hermes`) |

Most agents keep sessions as files; **Hermes** stores them as rows in a per-profile SQLite database. The tool opens each `state.db` read-only (respecting the gateway's WAL writes) and never writes to it — deletion goes through `hermes sessions delete`, which also clears the FTS index. Only interactive CLI sessions are shown (`source = cli`); channel, cron, and imported sessions have no meaningful working directory and are skipped, keeping Hermes in the same "sessions you ran in a directory" scope as Kiro and Claude Code. Since a DB-backed session has no file size, its size is the total byte length of its message content. Sessions are grouped by profile as well as cwd — the same directory under different Hermes profiles forms distinct groups, labelled `name <profile>` (the root database is the `default` profile). A Hermes session is lock-protected only when its profile's gateway is running and that session is the one the gateway currently holds.

Agent Session Butler is **read-only except for deletion** — it never modifies session content.

## Install / build

Requires Go 1.25+.

```bash
./install.sh
```

This builds `asbutler` and installs it to `~/.local/bin` (override with `BIN_DIR=...`). Run it again any time to upgrade. Then use `asbutler webui`, `asbutler list`, etc. from anywhere.

Or build in place without installing:

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
asbutler webui                # open the browser UI (default http://127.0.0.1:7788)
asbutler webui --addr :8080   # bind a different host:port
asbutler webui --no-open      # start the server without opening a browser
asbutler version              # print the version
asbutler help
```

`-a` / `--agent` matches the agent name by case-insensitive substring, so `-a claude` selects "Claude Code" — no need to type the full name. `-o` / `--orphans` keeps only groups whose working directory no longer exists — the prime cleanup candidates. Flags compose, and the summary line reflects the filtered set.

### Browser UI (`webui`)

`asbutler webui` starts a local HTTP server, opens it in your default browser (skip with `--no-open`), and serves a self-contained two-pane master-detail view. A resizable sidebar (drag its right edge; the width is remembered) lists every working directory with a fixed header of agent-filter chips and a directory search; Hermes groups are labelled with their profile (`name <profile>`). Selecting a directory shows its sessions in a sortable table (Title / Agent / Messages / Size / Modified / Session ID; hover a session id to see it in full, click to copy). The agent chips toggle which agents are shown — like the CLI's `--agent`, all discovered agents are on by default. A persistent summary strip at the top of the detail pane carries a disk-usage bar split per agent plus an orphaned segment, each with its size and share of the total. Message counts and titles resolve on demand when a directory is opened. Rows are multi-selectable (Select all) for batch delete, and single or batch deletes go behind a confirmation dialog; sessions held by a running agent are lock-protected. A dark/bright theme toggle is remembered across visits and defaults to the system preference. The frontend (a small Alpine.js app) and its assets are embedded into the binary via `go:embed`, so it needs no network access and ships as a single file. Same core as the CLI — nothing new touches session parsing or deletion.

## Project layout

```
cmd/asbutler/main.go          CLI entry point (list / rm / webui / version)
internal/agent/
  agent.go                    Agent interface, Session, streaming jsonl reader
  kiro.go                     KiroAgent (.json + .jsonl bundle)
  claude.go                   ClaudeCodeAgent (.jsonl; cwd read from contents)
  hermes.go                   HermesAgent (per-profile SQLite; delete via CLI)
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
