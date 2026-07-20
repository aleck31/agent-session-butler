// Command asbutler (Agent Session Butler) manages the on-disk chat sessions of
// your AI coding agents. Phase 1 ships the `list` and `rm` subcommands over a
// cross-platform core; `serve` (browser UI) comes later.
package main

import (
	"fmt"
	"os"
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
  asbutler list           List every session, grouped by working directory
  asbutler rm <id>...     Permanently delete sessions by id
  asbutler help           Show this help

`)
}

func cmdList(args []string) {
	verbose := false
	for _, a := range args {
		if a == "-v" || a == "--verbose" {
			verbose = true
		}
	}

	s := store.New()
	installed := s.InstalledAgents()
	if len(installed) == 0 {
		fmt.Println("No supported agents found on this machine.")
		return
	}
	groups := s.Scan()

	orphans := 0
	total := 0
	for _, g := range groups {
		if !g.CwdExists() {
			orphans++
		}
		total += len(g.Sessions)
	}
	fmt.Printf("Discovered %d agent(s): %v — %d session(s) across %d directories (%d orphaned)\n\n",
		len(installed), installed, total, len(groups), orphans)

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
