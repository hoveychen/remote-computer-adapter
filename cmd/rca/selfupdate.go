package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hoveychen/remote-computer-adapter/internal/deploy"
)

// cmdSelfUpdate replaces this binary with the current release build.
func cmdSelfUpdate(args []string) int {
	f := flag.NewFlagSet("self-update", flag.ContinueOnError)
	releaseBase := f.String("release-base", deploy.DefaultReleaseBase, "where to download release archives from")
	if e := f.Parse(args); e != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "self-update takes no arguments")
		return 2
	}
	path, e := deploy.SelfUpdate(*releaseBase, os.Stdout)
	if e != nil {
		fmt.Fprintln(os.Stderr, "self-update:", e)
		return 1
	}
	fmt.Printf("updated %s\n", path)
	return 0
}
