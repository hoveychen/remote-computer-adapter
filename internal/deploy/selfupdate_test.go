package deploy

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shell script stands in for the release build: SelfUpdate only needs
// something that runs and prints a version, and this keeps the test from
// depending on cross-compiling a real binary.
func fakeBuild(t *testing.T, body string) []byte {
	t.Helper()
	return []byte("#!/bin/sh\n" + body + "\n")
}

// installAt puts a runnable stand-in at path and returns it, so the test can
// update a binary that is not the test process itself.
func installAt(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunsHereRejectsABuildThatCannotExecute(t *testing.T) {
	dir := t.TempDir()
	// Not executable at all.
	notExec := filepath.Join(dir, "broken")
	if err := os.WriteFile(notExec, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runsHere(notExec); err == nil {
		t.Error("a non-executable build was accepted")
	}
	// Executable, but fails when asked for its version.
	failing := installAt(t, dir, "failing", fakeBuild(t, "exit 3"))
	if err := runsHere(failing); err == nil {
		t.Error("a build that exits non-zero was accepted")
	}
	// Executable and silent: a version command that prints nothing means the
	// binary is not the one we think it is.
	silent := installAt(t, dir, "silent", fakeBuild(t, "exit 0"))
	if err := runsHere(silent); err == nil {
		t.Error("a build that reports no version was accepted")
	}
	good := installAt(t, dir, "good", fakeBuild(t, "echo 'rca v1.2.3'"))
	if err := runsHere(good); err != nil {
		t.Errorf("a working build was rejected: %v", err)
	}
}

// The update must stop before downloading when the install directory is not
// ours, so the operator gets a permissions error rather than a wasted download
// and a failed rename.
func TestWritableRefusesADirectoryWeCannotWrite(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	if err := writable(filepath.Join(locked, "rca")); err == nil {
		t.Fatal("an unwritable install directory was accepted")
	}
	if err := writable(filepath.Join(dir, "rca")); err != nil {
		t.Errorf("a writable directory was rejected: %v", err)
	}
}

// The whole point of self-update is that the binary at the install path is
// replaced atomically and the replacement actually works.
func TestSelfUpdateReplacesTheBinary(t *testing.T) {
	dir := t.TempDir()
	current := installAt(t, dir, "rca", fakeBuild(t, "echo 'rca v0.0.1-old'"))

	next := fakeBuild(t, "echo 'rca v9.9.9-new'")
	archive := buildArchive(t, next, "rca")
	base := releaseServer(t, map[string][]byte{Local().Archive(): archive},
		sum(archive)+"  "+Local().Archive()+"\n")

	// Point at the stand-in rather than the test binary: replacing the test
	// process's own executable mid-run is not something to do for a test.
	updated, err := selfUpdateAt(current, base, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if updated != current {
		t.Errorf("updated %q, want %q", updated, current)
	}
	out, err := commandOutput(current, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "v9.9.9-new") {
		t.Fatalf("the old binary is still in place: %s", out)
	}
	if _, err := os.Stat(current + ".incoming"); !os.IsNotExist(err) {
		t.Error("the staging file was left behind")
	}
}

// A tampered release must leave the working binary exactly as it was.
func TestSelfUpdateKeepsTheOldBinaryOnAChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	original := fakeBuild(t, "echo 'rca v0.0.1-old'")
	current := installAt(t, dir, "rca", original)

	archive := buildArchive(t, fakeBuild(t, "echo new"), "rca")
	base := releaseServer(t, map[string][]byte{Local().Archive(): archive},
		strings.Repeat("0", 64)+"  "+Local().Archive()+"\n")

	if _, err := selfUpdateAt(current, base, io.Discard); err == nil {
		t.Fatal("a mismatched release was installed")
	}
	body, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatal("the working binary was modified")
	}
}

// A release that passes its checksum but cannot run must also be refused: the
// checksum only proves the bytes are the published ones.
func TestSelfUpdateKeepsTheOldBinaryWhenTheNewOneDoesNotRun(t *testing.T) {
	dir := t.TempDir()
	original := fakeBuild(t, "echo 'rca v0.0.1-old'")
	current := installAt(t, dir, "rca", original)

	archive := buildArchive(t, []byte("\x7fELF-but-not-really"), "rca")
	base := releaseServer(t, map[string][]byte{Local().Archive(): archive},
		sum(archive)+"  "+Local().Archive()+"\n")

	if _, err := selfUpdateAt(current, base, io.Discard); err == nil {
		t.Fatal("an unrunnable release was installed")
	}
	body, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatal("the working binary was modified")
	}
}
