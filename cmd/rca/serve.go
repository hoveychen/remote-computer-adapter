package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/hoveychen/remote-computer-adapter/internal/executor"
)

// cmdServe runs the untrusted sidecar on the remote host. It speaks the
// executor protocol on stdio, so the trusted side reaches it through whatever
// transport it execs — typically `ssh host rca serve --root /work/project`.
func cmdServe(args []string) int {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := f.String("root", "", "absolute directory this executor is confined to")
	if e := f.Parse(args); e != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(os.Stderr, "serve requires --root <absolute directory>")
		return 2
	}
	if e := executor.ServeStdio(context.Background(), *root); e != nil {
		fmt.Fprintln(os.Stderr, "serve:", e)
		return 1
	}
	return 0
}
