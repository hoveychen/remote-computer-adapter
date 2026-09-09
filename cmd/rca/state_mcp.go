package main

import (
	"flag"
	"fmt"
	"github.com/hoveychen/remote-computer-adapter/internal/trustedstate"
	"os"
)

func cmdStateMCP(args []string) int {
	f := flag.NewFlagSet("_state-mcp", flag.ContinueOnError)
	root := f.String("root", "", "trusted state root")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if *root == "" || f.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "_state-mcp requires --root <absolute private directory>")
		return 2
	}
	s, err := trustedstate.Open(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer s.Close()
	if err := trustedstate.Serve(os.Stdin, os.Stdout, s); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
