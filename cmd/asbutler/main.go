// Command asbutler (Agent Session Butler) manages the on-disk chat sessions of
// your AI coding agents. Phase 1 ships the `list` and `rm` subcommands over a
// cross-platform core; `serve` (browser UI) comes later.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/aleck/agent-session-butler/internal/agent"
	"github.com/aleck/agent-session-butler/internal/store"
)

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
