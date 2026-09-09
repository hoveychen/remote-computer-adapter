// Package runtimehome owns the private directory a trusted harness runs out
// of. Both engines need the same property: the config the harness reads must
// be one this process wrote, in a directory nothing else is writing to, and
// the directory must not be some pre-existing home whose contents we would be
// silently adopting.
package runtimehome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Prepare makes dir a private, exclusively locked runtime home containing the
// named subdirectories, and returns the lock file. Closing it releases the
// lock, so the caller must hold it for the harness's lifetime.
//
// marker identifies which harness owns the directory. A non-empty directory
// without that exact marker is refused rather than adopted: it may be a real
// CODEX_HOME or CLAUDE_CONFIG_DIR, and overwriting one would destroy the
// user's actual configuration.
func Prepare(dir, marker string, subdirs ...string) (*os.File, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("runtime home must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := requirePrivateDir(dir, "runtime home"); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	existing, err := os.ReadFile(filepath.Join(dir, ".rca-native"))
	if len(entries) > 0 && (err != nil || string(existing) != marker) {
		return nil, errors.New("refusing unmanaged nonempty runtime home; choose a new directory")
	}

	fd, err := unix.Open(filepath.Join(dir, ".lock"),
		unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "native runtime lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("runtime home is already in use")
	}
	if err := AtomicWrite(dir, ".rca-native", marker); err != nil {
		lock.Close()
		return nil, err
	}
	for _, name := range subdirs {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			lock.Close()
			return nil, err
		}
		if err := requirePrivateDir(path, "runtime "+name); err != nil {
			lock.Close()
			return nil, err
		}
	}
	return lock, nil
}

func requirePrivateDir(path, what string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a private directory (0700)", what)
	}
	return nil
}

// AtomicWrite replaces dir/name with data via a temp file and rename, so the
// harness never reads a half-written config.
func AtomicWrite(dir, name, data string) error {
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
