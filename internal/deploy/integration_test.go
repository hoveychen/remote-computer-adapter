package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The deploy flow is mostly orchestration, and orchestration is exactly what
// unit tests of its pieces do not cover: the ordering, the quoting, and
// whether the thing that lands actually answers. These tests run the real flow
// against ssh and scp shims that execute locally, so everything but the
// network hop is the production path — including the executor handshake, which
// runs the real rca serve.

const sshShim = `#!/bin/sh
# ssh [-T] <target> <command...>
[ "$1" = "-T" ] && shift
shift            # drop the target
exec /bin/sh -c "$*"
`

// The scp shim evals its destination, because real scp hands the remote half
// to a shell on the far side — quoting that works here is quoting that works
// there.
const scpShim = `#!/bin/sh
# scp -q <src> <target>:<dst>
[ "$1" = "-q" ] && shift
src=$1
eval "dst=${2#*:}"
exec cp "$src" "$dst"
`

// buildRCA compiles the real binary so the handshake talks to a real executor.
func buildRCA(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rca")
	cmd := exec.Command("go", "build", "-o", path, "../../cmd/rca")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building rca: %v: %s", err, out)
	}
	return path
}

func writeShim(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shimOptions(t *testing.T, binary string) (Options, string) {
	t.Helper()
	sandbox := t.TempDir()
	shims := filepath.Join(sandbox, "shims")
	if err := os.Mkdir(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{
		Target:     "sandbox-host",
		RemotePath: filepath.Join(sandbox, "bin", "rca"),
		Binary:     binary,
		SSH:        writeShim(t, shims, "ssh", sshShim),
		SCP:        writeShim(t, shims, "scp", scpShim),
		Out:        &strings.Builder{},
	}, sandbox
}

func TestDeployInstallsAndTheBinaryAnswers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := buildRCA(t)
	opts, sandbox := shimOptions(t, binary)
	work := filepath.Join(sandbox, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	opts.VerifyRoot = work

	platform, err := Run(opts)
	if err != nil {
		t.Fatalf("%v\n%s", err, opts.Out)
	}
	if platform != Local() {
		t.Errorf("platform = %v, want %v", platform, Local())
	}
	info, err := os.Stat(opts.RemotePath)
	if err != nil {
		t.Fatalf("nothing was installed: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("the installed binary is not executable")
	}
	log := opts.Out.(*strings.Builder).String()
	for _, want := range []string{"remote platform:", "installed at", "remote reports: rca",
		"executor handshake ok", "executor root resolves to"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	// The resolved root is what remote_root must be set to, so it has to be
	// reported resolved — not echoed back as the operator typed it.
	resolved, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log, resolved) {
		t.Errorf("log does not report the resolved root %q:\n%s", resolved, log)
	}
}

// Upgrading over a previous install is the common case, and the staged move is
// what keeps a half-copied binary from ever sitting at the install path.
func TestDeployUpgradesInPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := buildRCA(t)
	opts, sandbox := shimOptions(t, binary)
	if err := os.MkdirAll(filepath.Dir(opts.RemotePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.RemotePath, []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(opts); err != nil {
		t.Fatalf("%v\n%s", err, opts.Out)
	}
	body, err := os.ReadFile(opts.RemotePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "exit 9") {
		t.Fatal("the old binary is still in place")
	}
	if _, err := os.Stat(opts.RemotePath + ".incoming"); !os.IsNotExist(err) {
		t.Error("the staging file was left behind")
	}
	_ = sandbox
}

// A binary that copies but cannot run is the failure the version check exists
// to catch — silence here would hand the operator a broken executor.
func TestDeployFailsWhenTheInstalledBinaryDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken")
	if err := os.WriteFile(broken, []byte("not an executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts, _ := shimOptions(t, broken)
	if _, err := Run(opts); err == nil {
		t.Fatal("a binary that cannot run was reported as installed")
	} else if !strings.Contains(err.Error(), "does not run") {
		t.Errorf("err = %v, want the version check to have caught it", err)
	}
}

// The install path is the operator's, but it should survive a space rather
// than silently install somewhere else.
func TestDeployHandlesAPathWithSpaces(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := buildRCA(t)
	opts, sandbox := shimOptions(t, binary)
	opts.RemotePath = filepath.Join(sandbox, "my tools", "rca")
	if _, err := Run(opts); err != nil {
		t.Fatalf("%v\n%s", err, opts.Out)
	}
	if _, err := os.Stat(opts.RemotePath); err != nil {
		t.Fatalf("nothing installed at %q: %v", opts.RemotePath, err)
	}
}
