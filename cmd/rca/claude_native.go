package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hoveychen/remote-computer-adapter/internal/claudenative"
)

func cmdClaudeNative(args []string) int {
	f := flag.NewFlagSet("claude-native", flag.ContinueOnError)
	config := f.String("config", "", "trusted harness JSON config")
	if e := f.Parse(args); e != nil {
		return 2
	}
	if *config == "" {
		fmt.Fprintln(os.Stderr, "claude-native requires --config")
		return 2
	}
	c, e := claudenative.Load(*config)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 2
	}
	rest := f.Args()
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if e := claudenative.ValidateArgs(rest); e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 2
	}
	if e := claudenative.Run(c, rest); e != nil {
		fmt.Fprintln(os.Stderr, "claude-native:", e)
		return 1
	}
	return 0
}
