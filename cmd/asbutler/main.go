// Command asbutler (Agent Session Butler) manages the on-disk chat sessions of
// your AI coding agents, over a cross-platform core shared by the CLI (list/rm)
// and the browser UI (webui).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aleck/agent-session-butler/internal/agent"
	"github.com/aleck/agent-session-butler/internal/server"
	"github.com/aleck/agent-session-butler/internal/store"
	"github.com/aleck/agent-session-butler/internal/update"
	"github.com/aleck/agent-session-butler/internal/view"
)

// writeJSON prints v as indented JSON to stdout (the default machine-readable
// output for `list` and `rm`).
func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// version is the release version, printed by `asbutler version`.
const version = "0.8.1"

func main() {
	agent.Version = version // one authoritative version, shared with agents we call
	args := os.Args[1:]
	cmd := "list"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "list", "ls":
		cmdList(args)
	case "rm", "delete":
		cmdRm(args)
	case "mv", "move":
		cmdRelocate(args, false)
	case "cp", "copy":
		cmdRelocate(args, true)
	case "rename", "title":
		cmdRename(args)
	case "show", "cat":
		cmdShow(args)
	case "webui":
		cmdWebUI(args)
	case "version", "--version":
		cmdVersion()
	case "update", "upgrade":
		cmdUpdate(args)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `Agent Session Butler — manage your AI agents' chat sessions.

Output is JSON by default (for agents); add -H/--human for readable text.

Usage:
  asbutler list                 Sessions for the current working directory (= --path .)
  asbutler list --path <path>   Sessions for that directory only (no recursion into subdirs)
  asbutler list --all           Every session on this machine
  asbutler list -H              Human-readable listing, grouped by directory
  asbutler list -a <agent>      Only sessions from a matching agent (e.g. -a claude)
  asbutler list -o              Only orphaned directories (implies --all)
  asbutler rm <id>...           Delete sessions by id; prints JSON results (-H for text)
  asbutler mv <id>... <new-cwd> Move sessions to a new working directory
  asbutler cp <id>... <new-cwd> Copy sessions to a new working directory (fresh ids)
  asbutler rename <id> <title>  Set a session's title, in the agent's own metadata
  asbutler show <id>            Print a session's conversation (last 5 turns)
    [--store v1|v2]             Which store, when an id is in more than one
    [--tail N|--head N|--all]   How many turns; --all can be very large
    [--tools]                   Include full tool arguments and output
  asbutler webui [--addr host:port] [--no-open]  Open the local browser UI (default 127.0.0.1:7788)
  asbutler version              Print the version, and note a newer release
  asbutler update               Replace this binary with the latest release
  asbutler help                 Show this help

`)
}

// listOptions is the resolved form of `list`'s flags — the scope rules from
// ADR-0002 D2 are already applied, so pathFilter == "" means machine-wide.
type listOptions struct {
	human       bool
	orphansOnly bool
	agentFilter string
	pathFilter  string
}

// parseListArgs resolves `list`'s flags, applying the scope rules: --all and
// --path conflict, -o implies --all (orphans have no reachable directory to
// scope to), and the default scope is the current directory.
func parseListArgs(args []string) (listOptions, error) {
	var o listOptions
	all := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-H" || a == "--human":
			o.human = true
		case a == "-o" || a == "--orphans":
			o.orphansOnly = true
		case a == "--all":
			all = true
		case a == "-p" || a == "--path":
			// value is the next arg
			if i+1 >= len(args) {
				return o, fmt.Errorf("--path needs a value (e.g. --path ~/repos/foo)")
			}
			o.pathFilter = args[i+1]
			i++
		case strings.HasPrefix(a, "--path="):
			o.pathFilter = strings.TrimPrefix(a, "--path=")
		case strings.HasPrefix(a, "-p="):
			o.pathFilter = strings.TrimPrefix(a, "-p=")
		case a == "-a" || a == "--agent":
			// value is the next arg
			if i+1 >= len(args) {
				return o, fmt.Errorf("--agent needs a value (e.g. -a claude)")
			}
			o.agentFilter = args[i+1]
			i++
		case strings.HasPrefix(a, "--agent="):
			o.agentFilter = strings.TrimPrefix(a, "--agent=")
		case strings.HasPrefix(a, "-a="):
			o.agentFilter = strings.TrimPrefix(a, "-a=")
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
	}

	if all && o.pathFilter != "" {
		return o, fmt.Errorf("--all and --path are mutually exclusive")
	}
	// Orphan groups have no reachable directory to scope to, so they only make
	// sense machine-wide.
	if o.orphansOnly {
		all = true
	}
	// Default is the current directory; --all opts back into the whole machine.
	if !all && o.pathFilter == "" {
		o.pathFilter = "."
	}
	return o, nil
}

func cmdList(args []string) {
	opts, err := parseListArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(2)
	}
	human, orphansOnly := opts.human, opts.orphansOnly
	agentFilter, pathFilter := opts.agentFilter, opts.pathFilter

	s := store.New()
	installed := s.InstalledAgents()
	groups := s.Scan()

	// Narrow before enriching: enrichment reads every session file to count
	// messages, so scoping first is the difference between seconds and minutes.
	if pathFilter != "" {
		want, err := resolvePath(pathFilter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list: %s: %v\n", pathFilter, err)
			os.Exit(2)
		}
		filtered := groups[:0]
		for _, g := range groups {
			if samePath(g.Cwd, want) {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}

	// Filter each group's sessions by agent name (case-insensitive substring,
	// so `-a claude` matches "Claude Code"); drop groups left empty.
	if agentFilter != "" {
		needle := strings.ToLower(agentFilter)
		filtered := groups[:0]
		for _, g := range groups {
			kept := g.Sessions[:0]
			for _, sess := range g.Sessions {
				if strings.Contains(strings.ToLower(sess.Agent), needle) {
					kept = append(kept, sess)
				}
			}
			if len(kept) > 0 {
				g.Sessions = kept
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}

	// Keep only orphan groups (working directory gone) — the prime cleanup
	// candidates. Composes with --agent.
	if orphansOnly {
		filtered := groups[:0]
		for _, g := range groups {
			if !g.CwdExists() {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}

	if human {
		listHuman(s, installed, groups, agentFilter, orphansOnly)
		return
	}
	// Default: JSON for agents. Enrich everything so message counts and titles
	// are present (agents shouldn't get lazy nulls), then emit the flat view.
	for i := range groups {
		groups[i] = s.EnrichGroup(groups[i])
	}
	writeJSON(view.Flat(installed, groups, version))
}

// resolvePath turns a user-supplied path into the absolute, symlink-resolved
// form that groups are keyed by. Resolution is best-effort: a path that no
// longer exists still resolves to its absolute form so orphans stay queryable.
func resolvePath(p string) (string, error) {
	abs, err := store.ExpandPath(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// samePath reports whether a group's cwd is the directory the user asked for,
// tolerating symlinks (macOS /tmp) and case-insensitive filesystems.
func samePath(groupCwd, want string) bool {
	if pathEqual(filepath.Clean(groupCwd), want) {
		return true
	}
	// The agent may have recorded an unresolved path (/tmp/x vs /private/tmp/x).
	if resolved, err := filepath.EvalSymlinks(groupCwd); err == nil {
		return pathEqual(resolved, want)
	}
	return false
}

func pathEqual(a, b string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// listHuman renders the friendly grouped text output (asbutler list --human).
func listHuman(s *store.Store, installed []string, groups []store.Group, agentFilter string, orphansOnly bool) {
	if len(installed) == 0 {
		fmt.Println("No supported agents found on this machine.")
		return
	}
	if len(groups) == 0 {
		if orphansOnly {
			fmt.Println("No orphaned directories — every session's working directory still exists.")
		} else if agentFilter != "" {
			fmt.Printf("No sessions from an agent matching %q. Installed: %v\n", agentFilter, installed)
		} else {
			fmt.Println("No sessions found.")
		}
		return
	}

	orphans, total := 0, 0
	for _, g := range groups {
		if !g.CwdExists() {
			orphans++
		}
		total += len(g.Sessions)
	}
	scope := ""
	if agentFilter != "" {
		scope = fmt.Sprintf(" matching %q", agentFilter)
	}
	fmt.Printf("Discovered %d agent(s): %v — %d session(s)%s across %d directories (%d orphaned)\n\n",
		len(installed), installed, total, scope, len(groups), orphans)

	for _, g := range groups {
		badge := ""
		if !g.CwdExists() {
			badge = "  [missing]"
		}
		fmt.Printf("● %s%s  (%d sessions, %s)\n",
			g.Cwd, badge, len(g.Sessions), store.HumanSize(g.TotalSize()))
		g = s.EnrichGroup(g)
		// Render through the view layer so both surfaces agree — notably on
		// disambiguating titles that collide within a directory.
		printSessions(view.GroupView(g).Sessions)
		fmt.Println()
	}
}

func printSessions(sessions []view.Session) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tAGENT\tMSGS\tSIZE\tMODIFIED\tTITLE")
	for _, s := range sessions {
		msgs := "-"
		if s.MessageCount != nil {
			msgs = fmt.Sprintf("%d", *s.MessageCount)
		}
		id := s.ID
		if len(id) > 12 {
			id = id[:12]
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			id, s.Agent, msgs, s.SizeHuman,
			s.ModifiedAt.Format("2006-01-02 15:04"), s.Title)
	}
	tw.Flush()
}

// takeStoreFlag pulls --store out of an argument list. Kiro keeps two stores and
// the same id can be in both, so an operation on such an id has to say which.
func takeStoreFlag(cmd string, args []string) (rest []string, store string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--store":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "%s: --store needs a value (e.g. --store v1)\n", cmd)
				os.Exit(2)
			}
			store = args[i+1]
			i++
		case strings.HasPrefix(a, "--store="):
			store = strings.TrimPrefix(a, "--store=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, store
}

// rmResult is one id's deletion outcome (JSON output for agents).
type rmResult struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
	Error   string `json:"error,omitempty"`
}

func cmdRm(args []string) {
	args, storeName := takeStoreFlag("rm", args)
	human := false
	var ids []string
	for _, a := range args {
		switch a {
		case "-H", "--human":
			human = true
		default:
			ids = append(ids, a)
		}
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "rm: need at least one session id")
		os.Exit(2)
	}

	s := store.New()
	results := make([]rmResult, 0, len(ids))
	anyFail := false
	for _, id := range ids {
		err := s.DeleteByID(id, storeName)
		r := rmResult{ID: id, Deleted: err == nil}
		if err != nil {
			r.Error = err.Error()
			anyFail = true
		}
		results = append(results, r)
	}

	if human {
		for _, r := range results {
			if r.Deleted {
				fmt.Printf("✓ deleted %s\n", r.ID)
			} else {
				fmt.Fprintf(os.Stderr, "✗ %s: %s\n", r.ID, r.Error)
			}
		}
	} else {
		writeJSON(results)
	}
	if anyFail {
		os.Exit(1)
	}
}

// relocateResult is the JSON output of mv/cp.
type relocateResult struct {
	ID     string `json:"id"` // resulting session id (new id for cp)
	NewCwd string `json:"newCwd"`
	Copied bool   `json:"copied"`
	Error  string `json:"error,omitempty"`
}

// cmdRelocate handles `mv`/`cp <id>... <new-cwd>`; asCopy picks copy vs move.
// Any number of ids, like delete takes; and as with the shell's own mv, the
// final positional is the destination.
func cmdRelocate(args []string, asCopy bool) {
	name := "mv"
	if asCopy {
		name = "cp"
	}
	args, storeName := takeStoreFlag(name, args)
	human := false
	var pos []string
	for _, a := range args {
		if a == "-H" || a == "--human" {
			human = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		fmt.Fprintf(os.Stderr, "%s: usage: asbutler %s <session-id>... <new-cwd>\n", name, name)
		os.Exit(2)
	}
	ids, newCwd := pos[:len(pos)-1], pos[len(pos)-1]

	// Best-effort, reported per id. A locked session or an agent that cannot
	// relocate will always fail, and rejecting the whole batch over one of those
	// would leave no way to make progress; a move cannot be rolled back anyway,
	// so an all-or-nothing guarantee is not on offer either way.
	s := store.New()
	results := make([]relocateResult, 0, len(ids))
	anyFail := false
	for _, id := range ids {
		newID, resolved, err := s.RelocateByID(id, storeName, newCwd, asCopy)
		// Report the cwd it was actually filed under, not the string passed in —
		// the target is expanded and normalised on the way through.
		res := relocateResult{ID: newID, NewCwd: resolved, Copied: asCopy}
		if err != nil {
			res.ID = id
			res.NewCwd = newCwd
			res.Error = err.Error()
			anyFail = true
		}
		results = append(results, res)
	}

	if human {
		verb := "moved"
		if asCopy {
			verb = "copied"
		}
		for i, r := range results {
			switch {
			case r.Error != "":
				fmt.Fprintf(os.Stderr, "✗ %s: %s\n", r.ID, r.Error)
			case asCopy:
				// r.ID is the fresh copy's id, so name the source separately.
				fmt.Printf("✓ %s %s → %s (new id %s)\n", verb, ids[i], r.NewCwd, r.ID)
			default:
				fmt.Printf("✓ %s %s → %s\n", verb, r.ID, r.NewCwd)
			}
		}
	} else {
		writeJSON(results)
	}
	if anyFail {
		os.Exit(1)
	}
}

// renameResult is the JSON output of rename.
type renameResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Error string `json:"error,omitempty"`
}

// cmdRename handles `rename <id> <title>`. One session at a time on purpose:
// giving several sessions the same title would recreate the ambiguity renaming
// exists to remove.
func cmdRename(args []string) {
	args, storeName := takeStoreFlag("rename", args)
	human := false
	var pos []string
	for _, a := range args {
		if a == "-H" || a == "--human" {
			human = true
		} else {
			pos = append(pos, a)
		}
	}
	// Accept an unquoted multi-word title, as `hermes sessions rename` does.
	if len(pos) < 2 {
		fmt.Fprintln(os.Stderr, "rename: usage: asbutler rename <session-id> <title>")
		os.Exit(2)
	}
	id, title := pos[0], strings.Join(pos[1:], " ")

	written, err := store.New().RenameByID(id, storeName, title)
	res := renameResult{ID: id, Title: written}
	if err != nil {
		res.Title = title
		res.Error = err.Error()
	}

	if human {
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %s\n", id, err)
		} else {
			fmt.Printf("✓ renamed %s → %q\n", id, written)
		}
	} else {
		writeJSON(res)
	}
	if err != nil {
		os.Exit(1)
	}
}

// cmdVersion prints the version and, at most once a day, mentions a newer
// release. The check is best-effort and silent on failure: `version` must work
// offline. Never do this in `list` — its output is parsed by other tools.
func cmdVersion() {
	fmt.Printf("asbutler v%s\n", version)
	if rel, ok := update.Available(version); ok {
		fmt.Printf("\nA newer release is available: %s\n  %s\n  run `asbutler update` to install it\n",
			rel.Tag, rel.URL)
	}
}

// cmdUpdate replaces the running binary with the latest release.
func cmdUpdate(args []string) {
	check := false
	for _, a := range args {
		switch a {
		case "--check", "-n":
			check = true
		default:
			fmt.Fprintf(os.Stderr, "update: unknown flag %q\n", a)
			os.Exit(2)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rel, err := update.LatestRelease(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: could not reach the releases: %v\n", err)
		os.Exit(1)
	}
	latest, err := update.ParseVersion(rel.Tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: latest release %q is not a version\n", rel.Tag)
		os.Exit(1)
	}
	cur, err := update.ParseVersion(version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: this build's version %q is not a version\n", version)
		os.Exit(1)
	}

	switch {
	case latest.Newer(cur):
		fmt.Printf("v%s → %s\n  %s\n", cur, rel.Tag, rel.URL)
	case cur.Newer(latest):
		// A source build ahead of the last release; downloading would go backwards.
		fmt.Printf("Already newer than the latest release (v%s > %s); nothing to do.\n", cur, rel.Tag)
		return
	default:
		fmt.Printf("Already on the latest release (v%s).\n", cur)
		return
	}
	if check {
		return
	}

	path, err := update.Apply(ctx, rel.Tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Updated %s to %s\n", path, rel.Tag)
}

// transcriptResult is the JSON output of show.
type transcriptResult struct {
	ID    string       `json:"id"`
	Agent string       `json:"agent"`
	Cwd   string       `json:"cwd"`
	Title string       `json:"title"`
	Turns []agent.Turn `json:"turns"`
}

// cmdShow prints a session's conversation. Defaults to the last few turns
// with tool bodies folded — a whole session reaches 97 MB, so showing everything
// has to be asked for.
func cmdShow(args []string) {
	args, storeName := takeStoreFlag("show", args)
	human, bodies := false, false
	opts := agent.TranscriptOptions{Tail: agent.DefaultTurns}
	var id string

	for i := 0; i < len(args); i++ {
		a := args[i]
		num := func() int {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "show: %s needs a number\n", a)
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				fmt.Fprintf(os.Stderr, "show: %s needs a positive number, got %q\n", a, args[i])
				os.Exit(2)
			}
			return n
		}
		switch {
		case a == "-H" || a == "--human":
			human = true
		case a == "--tools":
			bodies = true
		case a == "--all":
			opts.Tail, opts.Head = 0, 0
		case a == "--tail":
			opts.Tail, opts.Head = num(), 0
		case a == "--head":
			opts.Head, opts.Tail = num(), 0
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "show: unknown flag %q\n", a)
			os.Exit(2)
		default:
			if id != "" {
				fmt.Fprintln(os.Stderr, "show: takes one session id")
				os.Exit(2)
			}
			id = a
		}
	}
	if id == "" {
		fmt.Fprintln(os.Stderr, "show: usage: asbutler show <session-id> [--tail N|--head N|--all] [--tools]")
		os.Exit(2)
	}
	opts.Bodies = bodies

	sess, turns, err := store.New().TranscriptByID(id, storeName, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "show: %v\n", err)
		os.Exit(1)
	}

	if human {
		printTranscript(sess, turns, bodies)
		return
	}
	writeJSON(transcriptResult{
		ID: sess.ID, Agent: sess.Agent, Cwd: sess.Cwd, Title: sess.Title,
		Turns: turns,
	})
}

// printTranscript renders the readable form: one block per turn, tools as a
// single indented line each unless bodies were requested.
func printTranscript(sess agent.Session, turns []agent.Turn, bodies bool) {
	fmt.Printf("%s  (%s · %s)\n%s\n\n", sess.Title, sess.Agent, store.HumanSize(sess.FileSize), sess.Cwd)
	if len(turns) == 0 {
		fmt.Println("(no conversation found)")
		return
	}
	for i, ex := range turns {
		if i > 0 {
			fmt.Println(strings.Repeat("─", 60))
		}
		for _, t := range ex.Messages {
			label := map[string]string{"user": "you ", "assistant": "asst", "system": "sys "}[t.Role]
			if label == "" {
				label = t.Role
			}
			when := ""
			if t.At != nil {
				if ts, err := time.Parse(time.RFC3339, *t.At); err == nil {
					when = "  " + ts.Local().Format("15:04:05")
				}
			}
			if t.Text != "" {
				fmt.Printf("▸ %s%s  %s\n", label, when, indentBody(t.Text, "         "))
			}
			if t.ThinkingChars > 0 {
				if bodies && t.Thinking != "" {
					fmt.Printf("  · thinking (%d chars)  %s\n", t.ThinkingChars, indentBody(t.Thinking, "        "))
				} else {
					fmt.Printf("  · thinking (%d chars)\n", t.ThinkingChars)
				}
			}
			for _, c := range t.Tools {
				fmt.Printf("  ⤷ %-12s %-48s %8s\n", c.Name, c.Summary, store.HumanSize(int64(c.OutputBytes)))
				if bodies {
					if c.Args != "" {
						fmt.Printf("      args: %s\n", indentBody(c.Args, "            "))
					}
					if c.Output != "" {
						fmt.Printf("      out:  %s\n", indentBody(c.Output, "            "))
					}
				}
			}
		}
	}
}

// indentBody keeps a multi-line body aligned under its label.
func indentBody(s, pad string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+pad)
}

func cmdWebUI(args []string) {
	addr := "127.0.0.1:7788"
	open := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--addr":
			if i+1 < len(args) {
				addr = args[i+1]
				i++
			} else {
				fmt.Fprintln(os.Stderr, "webui: --addr needs a value (e.g. --addr 127.0.0.1:7788)")
				os.Exit(2)
			}
		case strings.HasPrefix(a, "--addr="):
			addr = strings.TrimPrefix(a, "--addr=")
		case a == "--no-open":
			open = false
		}
	}

	// Bind the socket up front so we can open the browser only once it's ready.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "webui: %v\n", err)
		os.Exit(1)
	}
	url := "http://" + ln.Addr().String()
	fmt.Printf("Agent Session Butler — serving at %s  (Ctrl-C to stop)\n", url)
	if open {
		openBrowser(url)
	}

	srv := &http.Server{Handler: server.New(version).Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintf(os.Stderr, "webui: %v\n", err)
		os.Exit(1)
	}
}

// openBrowser launches the default browser at url, best-effort — a failure
// (headless/SSH box with no browser) is ignored, the server still runs.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, append(args, url)...).Start()
}
