package deploy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SelfUpdate replaces this executable with the current release build for this
// platform, and reports the path it replaced.
//
// The replacement is a rename onto the running binary's path, which is safe on
// Unix: the running process keeps the inode it already opened, and only the
// next launch sees the new bytes. Writing in place would corrupt the running
// process instead.
//
// The new binary is verified twice — once against the published checksum, and
// again by running it. A build that downloads cleanly but cannot execute here
// (wrong platform published under the right name, a corrupted release) must not
// become the installed binary.
func SelfUpdate(releaseBase string, out io.Writer) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve symlinks: a package manager may have linked rca into place, and
	// replacing the link rather than the target would leave the real binary
	// stale while looking updated.
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	return selfUpdateAt(self, releaseBase, out)
}

// selfUpdateAt is SelfUpdate against an explicit path, so tests can update a
// stand-in rather than the running test binary.
func selfUpdateAt(self, releaseBase string, out io.Writer) (string, error) {
	if releaseBase == "" {
		releaseBase = DefaultReleaseBase
	}
	o := &Options{Out: out, ReleaseBase: releaseBase}
	if err := writable(self); err != nil {
		return self, err
	}

	platform := Local()
	o.logf("updating %s (%s)", self, platform)
	staged, cleanup, err := obtain(*o, platform)
	if err != nil {
		return self, err
	}
	defer cleanup()

	if err := runsHere(staged); err != nil {
		return self, err
	}

	// Rename within the install directory so the move is atomic; a temp file on
	// another filesystem would fall back to a copy, which is exactly the
	// non-atomic replacement this avoids.
	next := self + ".incoming"
	if err := copyFile(staged, next, 0o755); err != nil {
		return self, err
	}
	if err := os.Rename(next, self); err != nil {
		os.Remove(next)
		return self, fmt.Errorf("replacing %s: %w", self, err)
	}
	return self, nil
}

// writable fails early when the install directory is not ours, so the update
// stops before downloading rather than after.
func writable(self string) error {
	dir := filepath.Dir(self)
	probe, err := os.CreateTemp(dir, ".rca-update-probe-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (try sudo, or reinstall with install.sh): %w", dir, err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

// runsHere executes the candidate. A downloaded binary that cannot run is the
// one failure a checksum cannot catch, because the checksum only proves the
// bytes are the ones that were published.
func runsHere(path string) error {
	out, err := commandOutput(path, "version")
	if err != nil {
		return fmt.Errorf("the downloaded build does not run here: %w", err)
	}
	if len(out) == 0 {
		return errors.New("the downloaded build reported no version")
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	body, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, body, mode)
}
