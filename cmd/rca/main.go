// Command rca is the single-binary CLI for remote-computer-adapter. One executable
// carries every role; the first argument picks it:
//
//	rca codex-native --config <file> -- exec ...
//	                         — trusted harness: Go owns the launch, the model's
//	                           general tools bind to a remote-only executor, and
//	                           memory/skills commit to the trusted state service
//	rca _native-transport ... — internal: exec the remote transport program with
//	                           a cleared environment (never run by hand)
//	rca _state-mcp ...       — internal: trusted-state MCP over stdio
//
// Example:
//
//	local$ rca codex-native --config ~/.config/rca/native.json -- exec "fix the test"
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

func main() { os.Exit(dispatch(os.Args[1:])) }

func dispatch(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "codex-native":
		return cmdCodexNative(args[1:])
	case "claude-native":
		return cmdClaudeNative(args[1:])
	case "_native-transport":
		return cmdNativeTransport(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "deploy":
		return cmdDeploy(args[1:])
	case "self-update":
		return cmdSelfUpdate(args[1:])
	case "codex-install":
		return cmdCodexInstall(args[1:])
	case "_state-mcp":
		return cmdStateMCP(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	case "version", "--version":
		fmt.Println(versionString())
		return 0
	}
	usage(os.Stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `rca — trusted agent harness with a remote-only executor

Usage:
  rca codex-native --config <file> -- exec [--json] -- <prompt>
                          run Codex as a trusted harness: Go owns the launch and
                          config, general file/exec tools reach only the remote
                          executor, and memory/skills commit to the trusted
                          state service with CAS + audit
  rca claude-native --config <file> -- "<prompt>"
                          run Claude Code as a trusted harness. Claude Code has
                          no state-backend seam, so its built-in tools are
                          removed outright and replaced by rca's own; the model
                          sees mcp__rca__ tool names
  rca serve --root <dir>  remote side: the untrusted executor, speaking the
                          executor protocol on stdio and confined to <dir>.
                          Run it through a transport, e.g.
                          ssh sandbox-host rca serve --root /work/project
  rca deploy <ssh-target> install or upgrade rca on the remote executor host:
                          detects its platform, verifies the download against
                          the published checksum, and proves the installed
                          binary answers. --verify-root <dir> also runs the
                          executor handshake and reports the resolved root
  rca codex-install       install or update the patched Codex build codex-native
                          needs. --from <dir|tarball> installs a local build
                          from scripts/build-codex-package.sh; otherwise the
                          release build for this platform is downloaded
  rca self-update         replace this binary with the current release build,
                          verified against the published checksum and by
                          running it before it takes over
  rca version             print version

The config file is JSON with absolute paths:
  binary        the Codex binary carrying the native-state patches
  runtime_home  private managed CODEX_HOME (0700, must be empty or rca-owned)
  state_root    trusted state service store
  remote_cwd    working directory on the remote executor
  exec_program  transport program, e.g. /usr/bin/ssh
  exec_args     its arguments, e.g. ["-T", "host", "codex exec-server --listen stdio"]
`)
}

// version is stamped by release builds via -ldflags "-X main.version=v1.2.3"
// (see scripts/build-release.sh); source builds fall back to module build info.
var version string

func versionString() string {
	if version != "" {
		return "rca " + version
	}
	v := "rca (devel)"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v = "rca " + bi.Main.Version
	}
	return v
}
