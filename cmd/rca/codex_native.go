package main

import (
	"flag"
	"fmt"
	"os"
	"syscall"

	"github.com/hoveychen/remote-adapter/internal/codexnative"
)

func cmdCodexNative(args []string) int {
	f := flag.NewFlagSet("codex-native", flag.ContinueOnError)
	config := f.String("config", "", "trusted prototype JSON config")
	if e := f.Parse(args); e != nil {
		return 2
	}
	if *config == "" {
		fmt.Fprintln(os.Stderr, "codex-native requires --config")
		return 2
	}
	c, e := codexnative.Load(*config)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 2
	}
	if e = codexnative.ValidateArgs(f.Args()); e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 2
	}
	self, e := os.Executable()
	if e == nil {
		e = codexnative.Run(c, f.Args(), self)
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, "codex-native:", e)
		return 1
	}
	return 0
}

// Invoked only by the generated environment registry. Replace this helper
// with the trusted transport, clearing harness credentials and injection env.
func cmdNativeTransport(args []string) int {
	if len(args) < 2 || args[0] != "--" || args[1] == "" || args[1][0] != '/' {
		fmt.Fprintln(os.Stderr, "_native-transport requires -- <absolute program> [args]")
		return 2
	}
	if e := syscall.Exec(args[1], args[1:], []string{"PATH=/usr/bin:/bin"}); e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	return 0
}
