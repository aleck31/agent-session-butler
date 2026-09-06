# Agent Session Butler

**Cross-platform chat-session manager for your AI coding agents.**

Agent Session Butler auto-discovers the AI coding agents installed on your machine (Kiro, Claude Code, …), groups their chat sessions by working directory, and lets you browse, sort, and clean them up — reclaiming disk space and keeping your session history tidy.

A single static Go binary. Runs on Linux, macOS, and Windows. Provides a terminal CLI and a local `webui` mode with a browser UI.

> This is the cross-platform successor to the macOS-only SwiftUI app *SessionSweep*. The portable core (agent discovery, session parsing, grouping, orphan detection, mtime-cached enrichment) carried over; the Apple-specific UI and packaging were dropped.

## Features

- **Auto-discovery** — finds installed agents and scans their on-disk sessions. Agents are peers; add one by implementing the `Agent` interface.
- **Grouped by working directory** — every `cwd` you've used an agent in, sorted by most recent activity, with per-directory session count and size.
- **Orphan detection** — directories that no longer exist (project deleted, sessions linger) are flagged `[missing]` — prime cleanup candidates.
- **Lazy enrichment** — message count and titles are computed on demand, so a bare listing stays instant even with tens of MB of `.jsonl`.
- **Move / copy sessions** — re-home a session to a new working directory when a repo moves (`~/ideas/foo` → `~/repos/foo`), so the agent can resume it at the new path. Move or copy, via CLI or the web UI. (Not supported for Hermes, whose home is its profile.)
- **Lock-aware** — sessions held by a **running** agent are lock-detected (live PID check) and protected from deletion or relocation.

### Supported agents

| Agent | Session storage |
|-------|-----------------|
| Kiro | files under `~/.kiro/sessions/cli/` |
| Claude Code | `~/.claude/projects/<encoded-cwd>/*.jsonl` |
| Hermes | SQLite `state.db` per profile under `$HERMES_HOME` (default `~/.hermes`) |

Most agents keep sessions as files; **Hermes** stores them as rows in a per-profile SQLite database. The tool opens each `state.db` read-only (respecting the gateway's WAL writes) and never writes to it — deletion goes through `hermes sessions delete`, which also clears the FTS index. Only interactive CLI sessions are shown (`source = cli`); channel, cron, and imported sessions have no meaningful working directory and are skipped, keeping Hermes in the same "sessions you ran in a directory" scope as Kiro and Claude Code. Since a DB-backed session has no file size, its size is the total byte length of its message content. Sessions are grouped by profile as well as cwd — the same directory under different Hermes profiles forms distinct groups, labelled `name <profile>` (the root database is the `default` profile). A Hermes session is lock-protected only when its profile's gateway is running and that session is the one the gateway currently holds.

Agent Session Butler only touches sessions when you ask: it deletes on request, and moves/copies a session's working-directory association on request (rewriting just the cwd, and for a copy a fresh id). It never alters conversation content.

## Install / build

```bash
./install.sh
```

Installs `asbutler` to `~/.local/bin` (override with `BIN_DIR=...`); rerun any time to upgrade. By default it downloads the matching prebuilt binary from the [latest release](https://github.com/aleck31/agent-session-butler/releases/latest) — no Go needed. Pass `--build` to compile the checked-out source instead (needs Go 1.25+; use this when you've changed the code). Then use `asbutler webui`, `asbutler list`, etc. from anywhere.

The installer prefers the GitHub CLI (`gh release download`) and falls back to an anonymous download, then to telling you to use `--build`. The `gh` path is first because it carries your credentials, which is what makes it work while the repo is private — anonymous release URLs 404 there. Either path works once the repo is public, so nothing needs changing then; `gh auth login` once is enough for now.

You can also grab a binary straight from the [releases page](https://github.com/aleck31/agent-session-butler/releases).

Or build in place without installing (needs Go):

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
asbutler list                 # sessions for the current directory, as JSON (= --path .)
asbutler list --path ~/foo    # sessions for that directory only (no recursion into subdirs)
asbutler list --all           # every session on this machine
asbutler list -H              # human-readable, grouped by working directory
asbutler list -a claude       # only a matching agent (case-insensitive substring)
asbutler list -o              # only orphaned directories (implies --all)
asbutler rm <id>...           # delete sessions by id; JSON results (-H for text)
asbutler mv <id> <new-cwd>    # move a session to a new working directory
asbutler cp <id> <new-cwd>    # copy a session to a new working directory (new id)
asbutler webui                # open the browser UI (default http://127.0.0.1:7788)
asbutler webui --addr :8080   # bind a different host:port
asbutler webui --no-open      # start the server without opening a browser
asbutler version              # print the version
asbutler help
```

Output is JSON by default (for agents); `-H`/`--human` gives readable text. `-a` / `--agent` matches the agent name by case-insensitive substring, so `-a claude` selects "Claude Code". `-o` / `--orphans` keeps only groups whose working directory no longer exists. `mv` / `cp` re-home a session so a relocated repo's session resumes at the new path (Hermes excluded).

`list` is scoped to one directory by default because listing is only cheap when it is: the JSON path enriches every session it returns (reading each file to count messages), so a machine-wide `--all` over ~1.5 GiB of history takes ~35s where a single directory takes ~2s. `--path` narrows *before* enrichment. Matching is on the exact directory — a parent does not pick up its children's sessions — and tolerates `~`, relative paths, symlinks (macOS `/tmp` → `/private/tmp`), and case-insensitive filesystems. A directory with no sessions is an empty result, not an error.

Note: before 0.6.1 `list` had no path filter and always returned every session. Pass `--all` for that behaviour. Since 0.6.2 an unrecognised flag is an error rather than being silently ignored — a typo like `--paths ~/foo` used to fall back to the current directory and quietly return the wrong scope.

### Downstream consumers

The `list` JSON is a contract, not just output — at least one tool parses it, and nothing else in this repo points at it, so it's recorded here.

| Consumer | How it calls | Fields it reads |
|---|---|---|
| [tabby-agent-sessions](https://github.com/aleck31/tabby-agent-sessions) (private) | `execFile` → `asbutler list --path <cwd>` | `agent`, `cwd`, `title`, `messageCount`, `sizeHuman`, `modifiedAt`, `locked`, `id` |

What that pins down, for anyone changing this code:

- **The `{summary, sessions[]}` shape and those key names.** Renaming or removing one breaks the consumer's sidebar. `internal/view/view_test.go` asserts the exact key set, so such a change fails the test rather than shipping silently — when you do mean it, update the test, bump the version, and tell the consumer.
- **`--path` must narrow *before* enrichment.** This is the whole point of ADR-0002 D2. Moving the filter after enrichment would take an interactive query from ~1s back to ~36s on a real machine, which is a performance regression the consumer feels directly.
- **`asbutler list` must keep defaulting to JSON** (since 0.5.4) and `--path` must keep its exact-directory, non-recursive semantics.
- Minimum version required by the consumer: **0.6.1**.

### Browser UI (`webui`)

`asbutler webui` starts a local HTTP server, opens it in your default browser (skip with `--no-open`), and serves a self-contained two-pane master-detail view. A resizable sidebar (drag its right edge; the width is remembered) lists every working directory with a fixed header of agent-filter chips and a directory search; Hermes groups are labelled with their profile (`name <profile>`). Selecting a directory shows its sessions in a sortable table (Title / Agent / Messages / Size / Modified / Session ID; hover a session id to see it in full, click to copy). The agent chips toggle which agents are shown — like the CLI's `--agent`, all discovered agents are on by default. A persistent summary strip at the top of the detail pane carries a disk-usage bar split per agent plus an orphaned segment, each with its size and share of the total. Message counts and titles resolve on demand when a directory is opened. Rows are multi-selectable (Select all) for batch delete, and single or batch deletes go behind a confirmation dialog; sessions held by a running agent are lock-protected. A dark/bright theme toggle is remembered across visits and defaults to the system preference. The frontend (a small Alpine.js app) and its assets are embedded into the binary via `go:embed`, so it needs no network access and ships as a single file. Same core as the CLI — nothing new touches session parsing or deletion.

## Project layout

```
cmd/asbutler/main.go          CLI entry point (list / rm / mv / cp / webui / version)
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
internal/view/
  view.go                     JSON serialization: Flat (CLI/agents) + Grouped (web UI)
internal/server/
  server.go                   HTTP routes over the store
  web/                        embedded UI (index.html + vendored alpine.min.js)
```

Only the process-lock check is platform-specific (build-tag split); everything else is shared.

Tests live beside the code (`go test ./...`). They run against sandboxed `HOME` / `HERMES_HOME` temp dirs and never touch real session data. The invariants they pin are the ones that were established empirically and are easy to break by accident: Claude's lossy project-dir encoding (cwd must come from file contents), the move-rewrites-both-dir-and-in-file-cwd rule, Hermes' cli-only scope and read-only access, the `(profile, cwd)` grouping key, stale-lock detection, the mtime cache, and the agent-facing JSON contract below.

## License

[MIT](LICENSE)
