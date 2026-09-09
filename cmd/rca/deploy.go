package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hoveychen/remote-computer-adapter/internal/deploy"
)

// cmdDeploy installs or upgrades rca on the remote executor host.
func cmdDeploy(args []string) int {
	f := flag.NewFlagSet("deploy", flag.ContinueOnError)
	remotePath := f.String("remote-path", deploy.DefaultRemotePath, "install path on the remote")
	binary := f.String("binary", "", "local rca to push instead of downloading a release; must match the remote platform")
	releaseBase := f.String("release-base", deploy.DefaultReleaseBase, "where to download release archives from")
	verifyRoot := f.String("verify-root", "", "directory on the remote to prove the executor works against")
	sshCmd := f.String("ssh", "ssh", "ssh command")
	scpCmd := f.String("scp", "scp", "scp command")
	if e := f.Parse(args); e != nil {
		return 2
	}
	if f.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "deploy requires exactly one ssh target, e.g. rca deploy sandbox-host")
		return 2
	}
	if _, e := deploy.Run(deploy.Options{
		Target:      f.Arg(0),
		RemotePath:  *remotePath,
		Binary:      *binary,
		ReleaseBase: *releaseBase,
		VerifyRoot:  *verifyRoot,
		SSH:         *sshCmd,
		SCP:         *scpCmd,
		Out:         os.Stdout,
	}); e != nil {
		fmt.Fprintln(os.Stderr, "deploy:", e)
		return 1
	}
	return 0
}
