// Command asbutler (Agent Session Butler) manages the on-disk chat sessions of
// your AI coding agents, over a cross-platform core shared by the CLI (list/rm)
// and the browser UI (webui).
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aleck/agent-session-butler/internal/agent"
	"github.com/aleck/agent-session-butler/internal/server"
	"github.com/aleck/agent-session-butler/internal/store"
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
const version = "0.6.0"

func main() {
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
	case "webui":
		cmdWebUI(args)
	case "version", "--version":
		fmt.Printf("asbutler v%s\n", version)
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
  asbutler list                 List all sessions as JSON (summary + flat array)
  asbutler list -H              Human-readable listing, grouped by directory
  asbutler list -a <agent>      Only sessions from a matching agent (e.g. -a claude)
  asbutler list -o              Only orphaned directories (working dir is gone)
  asbutler rm <id>...           Delete sessions by id; prints JSON results (-H for text)
  asbutler mv <id> <new-cwd>    Move a session to a new working directory
  asbutler cp <id> <new-cwd>    Copy a session to a new working directory (new id)
  asbutler webui [--addr host:port] [--no-open]  Open the local browser UI (default 127.0.0.1:7788)
  asbutler version              Print the version
  asbutler help                 Show this help

`)
}

func cmdList(args []string) {
	human := false
	orphansOnly := false
	agentFilter := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-H" || a == "--human":
			human = true
		case a == "-o" || a == "--orphans":
			orphansOnly = true
		case a == "-a" || a == "--agent":
			// value is the next arg
			if i+1 < len(args) {
				agentFilter = args[i+1]
				i++
			} else {
				fmt.Fprintln(os.Stderr, "list: --agent needs a value (e.g. -a claude)")
				os.Exit(2)
			}
		case strings.HasPrefix(a, "--agent="):
			agentFilter = strings.TrimPrefix(a, "--agent=")
		case strings.HasPrefix(a, "-a="):
			agentFilter = strings.TrimPrefix(a, "-a=")
		}
	}

	s := store.New()
	installed := s.InstalledAgents()
	groups := s.Scan()

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
		printSessions(g.Sessions)
		fmt.Println()
	}
}

func printSessions(sessions []agent.Session) {
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
			id, s.Agent, msgs, store.HumanSize(s.FileSize),
			s.ModifiedAt.Format("2006-01-02 15:04"), s.Title)
	}
	tw.Flush()
}

// rmResult is one id's deletion outcome (JSON output for agents).
type rmResult struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
	Error   string `json:"error,omitempty"`
}

func cmdRm(args []string) {
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
		err := s.DeleteByID(id)
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
	ID     string `json:"id"`     // resulting session id (new id for cp)
	NewCwd string `json:"newCwd"`
	Copied bool   `json:"copied"`
	Error  string `json:"error,omitempty"`
}

// cmdRelocate handles `mv`/`cp <id> <new-cwd>`; asCopy picks copy vs move.
func cmdRelocate(args []string, asCopy bool) {
	name := "mv"
	if asCopy {
		name = "cp"
	}
	human := false
	var pos []string
	for _, a := range args {
		if a == "-H" || a == "--human" {
			human = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		fmt.Fprintf(os.Stderr, "%s: usage: asbutler %s <session-id> <new-cwd>\n", name, name)
		os.Exit(2)
	}
	id, newCwd := pos[0], pos[1]

	s := store.New()
	newID, err := s.RelocateByID(id, newCwd, asCopy)
	res := relocateResult{ID: newID, NewCwd: newCwd, Copied: asCopy}
	if err != nil {
		res.ID = id
		res.Error = err.Error()
	}

	if human {
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %s\n", id, err)
		} else if asCopy {
			fmt.Printf("✓ copied %s → %s (new id %s)\n", id, newCwd, newID)
		} else {
			fmt.Printf("✓ moved %s → %s\n", id, newCwd)
		}
	} else {
		writeJSON(res)
	}
	if err != nil {
		os.Exit(1)
	}
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
