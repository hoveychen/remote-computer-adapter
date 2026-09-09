package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hoveychen/remote-computer-adapter/internal/deploy"
)

// cmdCodexInstall installs or updates the patched Codex build that
// codex-native needs.
func cmdCodexInstall(args []string) int {
	f := flag.NewFlagSet("codex-install", flag.ContinueOnError)
	source := f.String("from", "", "package directory or .tar.gz to install; empty downloads the release build")
	root := f.String("dir", deploy.DefaultCodexRoot(), "where installed builds are kept")
	releaseBase := f.String("release-base", deploy.DefaultReleaseBase, "where to download from")
	if e := f.Parse(args); e != nil {
		return 2
	}
	if f.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "codex-install takes no positional arguments")
		return 2
	}
	manifest, binary, e := deploy.InstallCodex(deploy.CodexInstallOptions{
		Source:      *source,
		Root:        *root,
		ReleaseBase: *releaseBase,
		Out:         os.Stdout,
	})
	if e != nil {
		fmt.Fprintln(os.Stderr, "codex-install:", e)
		return 1
	}
	fmt.Printf("\ncodex build %s (%d patches on upstream %s)\n",
		manifest.Short(), manifest.PatchCount, manifest.UpstreamBaseline[:12])
	fmt.Printf("set \"binary\" in your codex-native config to:\n  %s\n", binary)
	return 0
}
