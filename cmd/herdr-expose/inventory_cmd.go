package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/muthuishere/herdr-expose/internal/inventory"
)

// `herdr-expose agents` — what every agent on this machine is actually doing.
//
// `msg agents` lists what EXISTS. This says what it MEANS, which is what a
// caller acts on: who is mid-turn, who is free to take work, who is stopped on
// a dialog a human must answer, who stopped because something broke, and who
// has done nothing long enough to be worth closing.
//
// It is one HTTP call to the daemon, because the daemon is the only thing that
// has been watching long enough to say how long an agent has been idle. Herdr
// reports "idle", never "idle since 9:14".

// matching finds agents by name, session, pane id, working directory or title.
//
// A substring, case-insensitively, across all of them, because you remember an
// agent by whichever of those you last saw: "jevcli" is a directory, "research"
// is a name, "w12:p1" is a pane. Ranked so an exact or name match comes first,
// since that is the one you meant when several things contain the word.
func matching(all []inventory.Agent, q string) []inventory.Agent {
	q = strings.ToLower(strings.TrimSpace(q))
	type hit struct {
		a    inventory.Agent
		rank int
	}
	var hits []hit
	for _, a := range all {
		name, addr := strings.ToLower(a.Name), strings.ToLower(a.Address)
		switch {
		case addr == q || name == q:
			hits = append(hits, hit{a, 0})
		case name != "" && strings.Contains(name, q):
			hits = append(hits, hit{a, 1})
		case strings.Contains(addr, q):
			hits = append(hits, hit{a, 2})
		case strings.Contains(strings.ToLower(a.Cwd), q):
			hits = append(hits, hit{a, 3})
		case strings.Contains(strings.ToLower(a.Title), q):
			hits = append(hits, hit{a, 4})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].rank < hits[j].rank })
	out := make([]inventory.Agent, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.a)
	}
	return out
}

// localAddress is this agent as the inventory names it: session/name, with no
// machine prefix, because an inventory covers one machine.
func localAddress() string {
	session, pane := os.Getenv("HERDR_SESSION"), os.Getenv("HERDR_PANE_ID")
	if session == "" || pane == "" {
		return ""
	}
	return session + "/" + agentName(session, pane)
}

// whoami is the one question a list cannot answer: which of these is me.
func cmdWhoami(args []string) error {
	c, err := newMsgCtx()
	if err != nil {
		return err
	}
	session, pane := os.Getenv("HERDR_SESSION"), os.Getenv("HERDR_PANE_ID")
	me := inventory.Self{
		Machine: machineName(),
		Session: session,
		PaneID:  pane,
		Address: self(),
	}
	if session != "" && pane != "" {
		me.Name = agentName(session, pane)
	}
	if cwd, err := os.Getwd(); err == nil {
		me.Cwd = cwd
	}
	if hasFlag(args, "--json") {
		return json.NewEncoder(os.Stdout).Encode(me)
	}
	if pane == "" {
		fmt.Println("not inside a Herdr pane — HERDR_SESSION and HERDR_PANE_ID are unset.")
		fmt.Println("that is not a fault: a shell started outside Herdr has no agent identity.")
		return nil
	}
	fmt.Printf("you are   %s\n", me.Address)
	fmt.Printf("session   %s\n", me.Session)
	fmt.Printf("pane      %s\n", me.PaneID)
	if me.Cwd != "" {
		fmt.Printf("cwd       %s\n", me.Cwd)
	}
	fmt.Printf("\naddress other agents here as %s/<name>; `herdr-expose agents` lists them.\n", me.Session)
	_ = c
	return nil
}

func cmdAgents(args []string) error {
	c, err := newMsgCtx()
	if err != nil {
		return err
	}
	var sum inventory.Summary
	path := "/v1/inventory"
	// self() is machine/session/name; an inventory address is session/name,
	// because the inventory is of ONE machine. Send the form it can match, or
	// the asking agent never recognises itself in its own list.
	if me := localAddress(); me != "" {
		path += "?self=" + url.QueryEscape(me)
	}
	if err := c.local.do("GET", path, nil, &sum); err != nil {
		return err
	}
	if hasFlag(args, "--json") {
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		return e.Encode(sum)
	}

	only, query := "", ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--state" && i+1 < len(args):
			only = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-"):
			// a flag we already handled, or do not know
		case query == "":
			// A bare word is a SEARCH. Finding the agent you mean should not
			// require piping this through grep: grep matches the rendered
			// line, so it cannot see a cwd that was truncated for width, and
			// it answers with text when what you want is an address.
			query = args[i]
		}
	}
	if query != "" {
		agents := matching(sum.Agents, query)
		if len(agents) == 0 {
			fmt.Printf("no agent matches %q.\n", query)
			fmt.Println("searched name, session, pane id, working directory and title.")
			return nil
		}
		sum.Agents = agents
	}

	agents := sum.Agents
	sort.SliceStable(agents, func(i, j int) bool {
		// The ones needing a human first, then the busy, then the free. A list
		// sorted by pane id buries the only row anybody has to act on.
		rank := map[string]int{
			inventory.StateBlocked: 0, inventory.StateError: 1,
			inventory.StateWorking: 2, inventory.StateIdle: 3,
		}
		ri, rj := rank[agents[i].State], rank[agents[j].State]
		if ri != rj {
			return ri < rj
		}
		return agents[i].Address < agents[j].Address
	})

	fmt.Printf("%s — %d agents in %d session(s): %d working, %d free, %d blocked, %d stopped on an error\n\n",
		sum.Machine, sum.Counts.Total, len(sum.Sessions), sum.Counts.Working,
		sum.Counts.Idle, sum.Counts.Blocked, sum.Counts.Error)

	for _, a := range agents {
		if only != "" && a.State != only {
			continue
		}
		me := ""
		if a.Self {
			me = "  ← you"
		}
		if query != "" {
			// A search is usually "which one is in that directory?", so the
			// answer shows the directory rather than making you look it up.
			fmt.Printf("%-34s %-8s %-8s %s%s\n", a.Address, a.Kind, a.State, a.Cwd, me)
			continue
		}
		fmt.Printf("%-38s %-8s %-8s %s%s\n", a.Address, a.Kind, a.State, a.Reason, me)
	}

	if sum.Counts.Closable > 0 && only == "" {
		fmt.Printf("\n%d can be closed (idle longer than 30m). They are the ones marked \"safe to close\".\n",
			sum.Counts.Closable)
	}
	if sum.Counts.Blocked > 0 && only == "" {
		fmt.Println("a blocked agent is waiting on a DIALOG. A human answers it; another agent must not.")
	}
	return nil
}
