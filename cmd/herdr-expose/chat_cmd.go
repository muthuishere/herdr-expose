package main

import (
	"encoding/json"
	"fmt"
	"os"

	herdrexpose "github.com/muthuishere/herdr-expose"
	"github.com/muthuishere/herdr-expose/internal/chat"
	"github.com/muthuishere/herdr-expose/internal/config"
)

const chatUsage = `herdr-expose chat — reach these agents from a chat app

  chat status [--json]     every configured adapter, its state, and why
  chat list                the bundled adapters and the adapters directory
  chat seed                write the bundled adapters out (never overwrites)

An adapter is a COMMAND that speaks newline-delimited JSON on stdin and
stdout: "node telegram.js", "bun telegram.ts", "go run adapter.go",
"python3 discord.py". The platform does the hard half -- cleaning the screen,
folding code, waiting for the agent to finish, remembering what you were
already shown -- so an adapter only moves bytes.

NOTHING RUNS UNTIL TWO SWITCHES ARE ON, and both ship off:

  [chat]
  enabled = true

  [[chat.adapters]]
  id = "telegram"
  enabled = true
  command = "node telegram.js"
    [chat.adapters.env]
    HERDR_EXPOSE_TELEGRAM_TOKEN = "$HERDR_EXPOSE_TELEGRAM_TOKEN"

A credential is written as a REFERENCE -- $NAME -- never a value. The name is
not a secret and the value is read from the environment at use, so a config
file never holds a token even when it is backed up, synced or pasted into a
bug report.

Enabling is a config edit on purpose. A command that this daemon will execute,
and a bot that will answer strangers, are not things a subcommand should turn
on for you.`

func cmdChat(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status", "":
		return chatStatus(args)
	case "list":
		return chatList()
	case "seed":
		return chatSeed()
	case "help", "--help", "-h":
		fmt.Println(chatUsage)
		return nil
	default:
		fmt.Println(chatUsage)
		return fmt.Errorf("unknown: chat %s", sub)
	}
}

func chatStatus(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := config.ChatAdaptersDir()
	if err != nil {
		return err
	}
	sum := chat.Plan(cfg.Chat, dir, nil)

	if hasFlag(args, "--json") {
		b, err := json.MarshalIndent(sum, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return exitForSummary(sum)
	}

	if !sum.Enabled {
		fmt.Println("[chat] disabled — nothing will run")
	}
	if len(sum.Adapters) == 0 {
		fmt.Println("no adapters configured")
		fmt.Printf("adapters live in %s\n", dir)
		return nil
	}
	for _, a := range sum.Adapters {
		mark := " "
		if a.NeedsAttention() {
			mark = "!"
		}
		fmt.Printf("%s %-14s %-14s %s\n", mark, a.ID, a.State, a.Command)
		for _, p := range a.Problems {
			fmt.Printf("    %s\n", p)
		}
		for _, e := range a.Env {
			// The literal, never the value: this prints to a terminal that
			// gets screenshotted and pasted into issues.
			state := ""
			if e.Reference {
				state = "  (not set)"
				if e.Resolved {
					state = "  (set)"
				}
			}
			fmt.Printf("    %-28s %s%s\n", e.Key, e.Literal, state)
		}
	}
	fmt.Printf("\nadapters live in %s\n", dir)
	return exitForSummary(sum)
}

// exitForSummary makes `chat status` usable in a script: non-zero when
// something needs a person. "Enabled but broken" and "deliberately off" are
// different answers, and only the first is a failure.
func exitForSummary(sum chat.Summary) error {
	if sum.NeedsAttention() {
		os.Exit(1)
	}
	return nil
}

func chatList() error {
	dir, err := config.ChatAdaptersDir()
	if err != nil {
		return err
	}
	fmt.Println("bundled adapters:")
	for _, n := range herdrexpose.BundledChatAdapterNames() {
		fmt.Printf("  %s\n", n)
	}
	fmt.Printf("\nadapters directory: %s\n", dir)
	fmt.Println("a copy of each bundled adapter is written there on daemon start,")
	fmt.Println("and an existing file is NEVER overwritten — delete one to get it back.")
	return nil
}

func chatSeed() error {
	res, err := config.SeedChatAdapters(herdrexpose.BundledChatAdapters())
	if err != nil {
		return err
	}
	for _, p := range res.Wrote {
		fmt.Println("wrote", p)
	}
	for _, p := range res.Kept {
		fmt.Println("kept ", p, " (yours — not overwritten)")
	}
	return nil
}
