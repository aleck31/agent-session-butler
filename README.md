# Agent Session Butler

**Cross-platform chat-session manager for your AI coding agents.**

Agent Session Butler auto-discovers the AI coding agents installed on your machine (Kiro, Claude Code, Codex, Hermes), groups their chat sessions by working directory, and lets you browse, sort, and clean them up — reclaiming disk space and keeping your session history tidy.

A single static Go binary. Runs on Linux, macOS, and Windows. Provides a terminal CLI and a local `webui` mode with a browser UI.

> This is the cross-platform successor to the macOS-only SwiftUI app *SessionSweep*. The portable core (agent discovery, session parsing, grouping, orphan detection, mtime-cached enrichment) carried over; the Apple-specific UI and packaging were dropped.

## Features

- **Auto-discovery** — finds installed agents and scans their on-disk sessions. Agents are peers; add one by implementing the `Agent` interface.
- **Grouped by working directory** — every `cwd` you've used an agent in, sorted by most recent activity, with per-directory session count and size.
- **Orphan detection** — directories that no longer exist (project deleted, sessions linger) are flagged `[missing]` — prime cleanup candidates.
- **Lazy enrichment** — message count and titles are computed on demand, so a bare listing stays instant even with tens of MB of `.jsonl`.
- **Move / copy sessions** — re-home a session to a new working directory when a repo moves (`~/ideas/foo` → `~/repos/foo`), so the agent can resume it at the new path. Move or copy, via CLI or the web UI. (Not supported for Hermes or Codex, whose cwd lives somewhere this tool only reads.)
- **View a session** — read the conversation without opening the agent: `asbutler show <id>`, or click the eye icon in the web UI. Defaults to the last few turns with tool output folded, because sessions reach 97 MB.
- **Rename sessions** — a title is the session's first prompt, so resuming the same task leaves several rows that read identically. Give one a name you'll recognise, written into the agent's own metadata so the agent shows it too. Click the title in the web UI, or `asbutler rename <id> <title>`.
- **Lock-aware** — where the agent exposes a lock, sessions held by a **running** agent are detected (live PID check) and protected from deletion or relocation. Codex exposes none, so its deletion safety is delegated to `codex delete`.

### Supported agents

| Agent | Session storage |
|-------|-----------------|
| Kiro | two stores: files under `~/.kiro/sessions/cli/` (v2) and rows in `data.sqlite3` (v1) |
| Claude Code | `~/.claude/projects/<encoded-cwd>/*.jsonl` |
| Codex | rollout `.jsonl` files indexed by `threads` in `$CODEX_HOME/state_<n>.sqlite` (default `~/.codex`) |
| Hermes | SQLite `state.db` per profile under `$HERMES_HOME` (default `~/.hermes`) |

Most agents keep sessions as files; **Hermes** stores them as rows in a per-profile SQLite database. The tool opens each `state.db` read-only (respecting the gateway's WAL writes) and never writes to it — deletion goes through `hermes sessions delete`, which also clears the FTS index. Only interactive CLI sessions are shown (`source = cli`); channel, cron, and imported sessions have no meaningful working directory and are skipped, keeping Hermes in the same "sessions you ran in a directory" scope as Kiro and Claude Code. Since a DB-backed session has no file size, its size is the total byte length of its message content. Sessions are grouped by profile as well as cwd — the same directory under different Hermes profiles forms distinct groups, labelled `name <profile>` (the root database is the `default` profile). A Hermes session is lock-protected only when its profile's gateway is running and that session is the one the gateway currently holds.

**Kiro** keeps two session stores and the same id can be in both: opening a v1 session copies it into v2 under that id and leaves the original behind. Rows therefore carry a `store` of `v1` or `v2` — the names Kiro's own CLI uses — and a shared id appears once per store, so the stale copy is visible and can be cleaned up. The store is not part of the grouping key: a directory's v1 and v2 sessions sit next to each other, which is how you spot the duplicate.

Because an id can match two sessions, `rm`, `mv`, `cp`, `rename` and `show` accept `--store v1|v2`, and refuse rather than guess when an id is ambiguous. v1 sessions can be listed, read and deleted; renaming and relocating are refused, because v1 stores no title (Kiro shows the first message) and keys conversations by cwd inside its own database.

**Codex** is a hybrid: conversation content lives in per-session rollout files (`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`) while an index of every session lives in the `threads` table of `$CODEX_HOME/state_<n>.sqlite`. The index is authoritative — Codex is migrating off the files, as its own `migrate-rollouts` command implies — so scanning reads only the DB (read-only, one indexed query, no file reads) and a rollout is opened only to count messages. Deletion shells out to `codex delete`, because removing a rollout file directly would leave a dangling `threads` row that Codex's session picker would list as a session whose content is gone. The `_<n>` suffix is a schema version Codex bumps, so the newest `state_<n>.sqlite` is selected rather than a fixed name.

Every Codex session is listed, including the subagent threads Codex spawns for itself (guardian reviews, spawned children). Unlike Hermes' out-of-scope sessions, these carry a real working directory and real bytes, and Codex has no retention policy that ever reclaims them — on the dev machine they were 31% of Codex's disk use, so hiding them would hide exactly the garbage this tool exists to surface. Two consequences of how Codex stores things: sessions have no per-session lock (only a global zero-byte coordination file with no PID), so none are reported as locked and `codex delete` is left to refuse a live session; and relocation is unsupported, since `cwd` sits in the read-only index as well as the rollout file with no CLI to change it. A session still open in the desktop app is invisible until Codex flushes it to the index — the `codex` CLI can't see it either.

Agent Session Butler only touches sessions when you ask: it deletes on request, and moves/copies a session's working-directory association on request (rewriting just the cwd, and for a copy a fresh id). It never alters conversation content.

## Install / build

```bash
./install.sh
```

Installs `asbutler` to `~/.local/bin` (override with `BIN_DIR=...`); rerun any time to upgrade. By default it downloads the matching prebuilt binary from the [latest release](https://github.com/aleck31/agent-session-butler/releases/latest) — no Go needed. Pass `--build` to compile the checked-out source instead (needs Go 1.25+; use this when you've changed the code). Then use `asbutler webui`, `asbutler list`, etc. from anywhere.

The installer stages the new binary beside the target and renames it into place, rather than writing over the existing one — on macOS, overwriting an executable in place invalidates its cached code signature and the kernel kills it on the next run. It prefers the GitHub CLI (`gh release download`) and falls back to an anonymous download, then to telling you to use `--build`. The `gh` path is first because it carries your credentials, which is what makes it work while the repo is private — anonymous release URLs 404 there. Either path works once the repo is public, so nothing needs changing then; `gh auth login` once is enough for now.

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
asbutler mv <id>... <new-cwd> # move sessions to a new working directory
asbutler cp <id>... <new-cwd> # copy sessions to a new working directory (fresh ids)
asbutler rename <id> <title>  # set a session's title, in the agent's own metadata
asbutler webui                # open the browser UI (default http://127.0.0.1:7788)
asbutler webui --addr :8080   # bind a different host:port
asbutler webui --no-open      # start the server without opening a browser
asbutler show <id>            # the conversation: last 5 turns, tools folded
asbutler show <id> --tail 20  # more turns (--head N from the start, --all for everything)
asbutler show <id> --tools    # include full tool arguments and output
asbutler update               # replace this binary with the latest release
asbutler update --check       # report whether a newer release exists, install nothing
asbutler version              # print the version, and note a newer release
asbutler help
```

Output is JSON by default (for agents); `-H`/`--human` gives readable text. `-a` / `--agent` matches the agent name by case-insensitive substring, so `-a claude` selects "Claude Code". `-o` / `--orphans` keeps only groups whose working directory no longer exists. `mv` / `cp` re-home sessions so a relocated repo's sessions resume at the new path (Hermes and Codex excluded). They take any number of ids with the destination last, and report per id like `rm` does — a locked session or an unsupported agent fails on its own without stranding the rest. The target is expanded (`~`, relative paths) and must be an existing directory: a typo would otherwise be stored verbatim and quietly turn the session into an orphan no directory query can reach.

`list` is scoped to one directory by default because listing is only cheap when it is: the JSON path enriches every session it returns (reading each file to count messages), so a machine-wide `--all` over ~1.5 GiB of history takes ~35s where a single directory takes well under a second. `--path` narrows *before* enrichment. Matching is on the exact directory — a parent does not pick up its children's sessions — and tolerates `~`, relative paths, symlinks (macOS `/tmp` → `/private/tmp`), and case-insensitive filesystems. A directory with no sessions is an empty result, not an error.

Titles come from a session's first prompt, so resuming the same task repeatedly leaves several sessions with the same title. Where titles collide **within one directory**, a short session-id suffix is appended (`… · d6a4de`) so they can be told apart; unique titles are left alone. `title` is a display string — use `id` as the identity.

There are benchmarks for where a scan's time actually goes, per agent and end to end. They read real session data, so they only run when asked:

```bash
go test ./internal/agent/ -bench Scan -benchtime 5x -run '^$'   # per agent
go test ./internal/store/ -bench Scan -benchtime 5x -run '^$'   # whole scan, cold and warm cache
```

Note: before 0.6.1 `list` had no path filter and always returned every session. Pass `--all` for that behaviour. Since 0.6.2 an unrecognised flag is an error rather than being silently ignored — a typo like `--paths ~/foo` used to fall back to the current directory and quietly return the wrong scope.

### Viewing a session

```bash
asbutler show <id>            # JSON, for scripts and agents
asbutler show <id> -H         # readable text
```

The unit is a **turn** — one message from you plus everything the agent did before you spoke again — not a message. That distinction matters: measured against real history, one turn holds 3 to 530 messages, so "the last 30 messages" can land in the middle of a single agent loop. The default is the last 5 turns, which is 2.6–77 KB of text out of sessions that run to tens of megabytes.

Tool arguments and output are folded to a one-line summary with their byte counts, and reasoning to a character count; `--tools` includes the bodies, clipped at 8 KB each. Base64 images render as `[image]` rather than being inlined.

What each agent can supply differs, and these gaps are in the data rather than unimplemented:

| Agent | Per-message time | Reasoning |
|-------|------------------|-----------|
| Kiro | not recorded | available |
| Claude Code | available | available |
| Codex | available | encrypted by Codex |
| Hermes | available | not stored |

### Updating

`asbutler update` downloads the release asset for your platform and replaces the running binary. It stages the download beside the target and renames it into place, and runs the new binary's own `version` before trusting it — so a truncated or wrong-platform download fails while the working binary is still there. A build newer than the latest release reports that and stops, rather than downgrading itself.

`asbutler version` also mentions a newer release when there is one. That check is bounded to one network call per day with a three-second timeout, and is silent on any failure — `version` works offline. Set `ASBUTLER_NO_UPDATE_CHECK=1` to switch it off. `list` never checks: its output is parsed by other tools, so it stays purely local.

While the repo is private both paths need the GitHub CLI, for the same reason `install.sh` does.

### Renaming

Titles come from a session's first prompt, which makes several sessions on one task indistinguishable. `rename` replaces the title in the agent's own store, so the change is visible in that agent too rather than only here. Every backend has an owner-blessed way to do it, and none of these write around the agent:

| Agent | How the title is set |
|-------|----------------------|
| Kiro | the `title` field in the session's `.json` |
| Claude Code | the `ai-title` rows in the session's `.jsonl` |
| Codex | the app-server's JSON-RPC `thread/name/set` |
| Hermes | `hermes sessions rename` |

Two caveats worth knowing. Claude Code re-emits its own `ai-title` row as a session progresses, so resuming a renamed session may let it supersede your title (it never *changes* an existing value — verified across 47 real sessions — it just appends its own again). And `rename` takes one session at a time on purpose: giving several the same title would recreate the ambiguity it exists to remove.

There is no length limit. Agents store far longer titles themselves — a Codex title is the raw first prompt, nearly 10,000 characters in one real case — so capping the input would make it impossible to put back a title that was already there. Clamping is done where titles are displayed.

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

`asbutler webui` starts a local HTTP server, opens it in your default browser (skip with `--no-open`), and serves a self-contained two-pane master-detail view. A resizable sidebar (drag its right edge; the width is remembered) lists every working directory with a fixed header of agent-filter chips and a directory search; Hermes groups are labelled with their profile (`name <profile>`). Selecting a directory shows its sessions in a sortable table (Title / Agent / Messages / Size / Modified / Session ID; hover a session id to see it in full, click to copy). The agent chips toggle which agents are shown — like the CLI's `--agent`, all discovered agents are on by default. A persistent summary strip at the top of the detail pane carries a disk-usage bar split per agent plus an orphaned segment, each with its size and share of the total. Message counts and titles resolve on demand when a directory is opened. Click the eye icon on a row to read its conversation. The panel opens on the last 5 turns with tool output folded; a button at the bottom loads five earlier turns at a time, holding your place rather than jumping. Message text is rendered as Markdown — code blocks, tables and lists included — and your turns are set apart from the agent's by shape, not just a label. Turns that carry only a tool result render nothing at all: in one real session 57% of the raw turns were such carriers. Click a session's title to rename it in place (Enter commits, Escape cancels). Rows are multi-selectable (Select all) for batch delete and batch move/copy, both behind a confirmation dialog; sessions held by a running agent are lock-protected. The batch move button counts only the sessions that can actually be relocated and says how many it skipped. A dark/bright theme toggle is remembered across visits and defaults to the system preference. The frontend (a small Alpine.js app) and its assets are embedded into the binary via `go:embed`, so it needs no network access and ships as a single file. Same core as the CLI — nothing new touches session parsing or deletion.

## Project layout

```
cmd/asbutler/main.go          CLI entry point (list / rm / mv / cp / webui / version)
internal/agent/
  agent.go                    Agent interface, Session, streaming jsonl reader
  kiro.go                     KiroAgent (.json + .jsonl bundle)
  claude.go                   ClaudeCodeAgent (.jsonl; cwd read from contents)
  codex.go                    CodexAgent (SQLite thread index + rollout files; delete via CLI)
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
  web/                        embedded UI; see web/README.md
    index.html                the whole app (Alpine.js, inline SVG icons)
    alpine.min.js             vendored, not fetched from a CDN
    md.js                     Markdown renderer for transcripts (escapes first)
    md_test.mjs               browser test for md.js, incl. XSS resistance
```

Only the process-lock check is platform-specific (build-tag split); everything else is shared.

Tests live beside the code (`go test ./...`). They run against sandboxed `HOME` / `HERMES_HOME` temp dirs and never touch real session data. The invariants they pin are the ones that were established empirically and are easy to break by accident: Claude's lossy project-dir encoding (cwd must come from file contents), the move-rewrites-both-dir-and-in-file-cwd rule, Hermes' cli-only scope and read-only access, Codex's schema-versioned state DB and turn-counting rules, the `(profile, cwd)` grouping key, stale-lock detection, the mtime cache, and the agent-facing JSON contract below.

## CI

Checks run in two places, and both call the same script so they cannot disagree:

```bash
./scripts/check.sh            # gofmt, vet, tests, all five release targets — ~8s
./scripts/check.sh --quick    # skip the cross-compiles
```

Run it before pushing. There is deliberately **no git hook** to do that for you: the only way to make git run a hook out of a clone is `core.hooksPath`, and on a managed machine that setting may already belong to something else — here it is claimed at system level by a corporate secret-scanning tool, and pointing it at a repo-local directory silently disables it. A convenience check is not worth turning that off, so this stays a command you run.

Two workflows under `.github/workflows/`:

- **CI** (push to master, PRs): runs `scripts/check.sh`, plus a per-target build matrix so a failure names the platform.
- **Release** (on a `v*` tag): builds the five assets `install.sh` expects and publishes them.

Both run on Linux only. Go cross-compiles without the target platform and the SQLite driver is pure Go (CGO off), so every release binary is produced there; a macOS runner would cost roughly ten times as much on a private repo for coverage of two small build-tagged branches. The trade-off — those branches are compile-checked, not behaviour-tested — is recorded in the development notes.

The release job refuses to publish when the tag disagrees with `const version` in `cmd/asbutler/main.go`, since `install.sh` compares those two to decide whether to download or build. Release notes are generated from the commit history; replace them with `gh release edit <tag> --notes-file notes.md` when a release deserves a written summary.

## License

[MIT](LICENSE)
