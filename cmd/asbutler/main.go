// Command asbutler (Agent Session Butler) manages the on-disk chat sessions of
// your AI coding agents, over a cross-platform core shared by the CLI (list/rm)
// and the browser UI (webui).
package main

import (
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
)

// version is the release version, printed by `asbutler version`.
const version = "0.5.3"

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

Usage:
  asbutler list                 List every session, grouped by working directory
  asbutler list -a <agent>      Only sessions from a matching agent (e.g. -a claude)
  asbutler list -o              Only orphaned directories (working dir is gone)
  asbutler list -v              Also show per-session details (message count, title)
  asbutler rm <id>...           Permanently delete sessions by id
  asbutler webui [--addr host:port] [--no-open]  Open the local browser UI (default 127.0.0.1:7788)
  asbutler version              Print the version
  asbutler help                 Show this help

`)
}

func cmdList(args []string) {
	verbose := false
	orphansOnly := false
	agentFilter := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-v" || a == "--verbose":
			verbose = true
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
	if len(installed) == 0 {
		fmt.Println("No supported agents found on this machine.")
		return
	}
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
		if len(groups) == 0 {
			fmt.Printf("No sessions from an agent matching %q. Installed: %v\n", agentFilter, installed)
			return
		}
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
		if len(groups) == 0 {
			fmt.Println("No orphaned directories — every session's working directory still exists.")
			return
		}
	}

	orphans := 0
	total := 0
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

		if verbose {
			g = s.EnrichGroup(g)
			printSessions(g.Sessions)
			fmt.Println()
		}
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

func cmdRm(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "rm: need at least one session id")
		os.Exit(2)
	}
	wanted := map[string]bool{}
	for _, id := range args {
		wanted[id] = true
	}

	s := store.New()
	groups := s.Scan()

	found := 0
	for _, g := range groups {
		for _, sess := range g.Sessions {
			if !wanted[sess.ID] {
				continue
			}
			found++
			if err := s.Delete(sess); err != nil {
				fmt.Fprintf(os.Stderr, "✗ %s: %v\n", sess.ID, err)
				continue
			}
			fmt.Printf("✓ deleted %s (%s, %s)\n", sess.ID, sess.Agent, store.HumanSize(sess.FileSize))
		}
	}
	if found == 0 {
		fmt.Fprintln(os.Stderr, "rm: no matching sessions found")
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
