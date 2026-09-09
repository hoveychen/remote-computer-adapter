package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CodexManifest is what build-codex-package.sh records alongside the binaries.
// It is the package's account of itself: which upstream commit it started
// from, which build it actually is, and what the binary hashes to.
type CodexManifest struct {
	Upstream         string `json:"upstream"`
	UpstreamBaseline string `json:"upstream_baseline"`
	BuiltFrom        string `json:"built_from"`
	PatchCount       int    `json:"patch_count"`
	BinarySHA256     string `json:"binary_sha256"`
	BuiltOn          string `json:"built_on"`
	License          string `json:"license"`
}

const manifestName = "rca-codex-manifest.json"

// Short is the build identifier used for the install directory.
func (m CodexManifest) Short() string {
	if len(m.BuiltFrom) >= 12 {
		return m.BuiltFrom[:12]
	}
	return m.BuiltFrom
}

func (m CodexManifest) validate() error {
	if m.BuiltFrom == "" {
		return errors.New("manifest has no built_from commit")
	}
	if m.UpstreamBaseline == "" {
		return errors.New("manifest has no upstream_baseline")
	}
	if len(m.BinarySHA256) != 64 {
		return errors.New("manifest has no usable binary_sha256")
	}
	if m.PatchCount <= 0 {
		// A package with no patches is stock Codex, which fails the
		// audited-native handshake. Installing it would look like an upgrade
		// and behave like a removal.
		return errors.New("manifest claims no patches; this is not a native-state build")
	}
	return nil
}

// CodexInstallOptions describes one install.
type CodexInstallOptions struct {
	// Source is a package directory or a .tar.gz. Empty downloads the release
	// build for this platform.
	Source string
	// Root is where versions are kept.
	Root string
	// ReleaseBase overrides the download location.
	ReleaseBase string
	Out         io.Writer
}

// DefaultCodexRoot is where installed Codex builds live.
func DefaultCodexRoot() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "rca", "codex")
	}
	return filepath.Join(os.TempDir(), "rca-codex")
}

// CodexArchive is the release asset name for a platform.
func (p Platform) CodexArchive() string { return "codex-native_" + p.OS + "_" + p.Arch + ".tar.gz" }

// InstallCodex places a patched Codex build under Root and points `current` at
// it, returning the manifest and the path to the binary.
//
// Installs are versioned by the commit they were built from and the `current`
// symlink is moved last, so a half-extracted package is never the one a
// harness resolves, and the previous build stays intact and reachable if this
// one turns out to be wrong.
func InstallCodex(o CodexInstallOptions) (CodexManifest, string, error) {
	if o.Root == "" {
		o.Root = DefaultCodexRoot()
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	logf := func(format string, args ...any) { fmt.Fprintf(o.Out, format+"\n", args...) }

	staging, cleanup, err := stageCodex(o, logf)
	if err != nil {
		return CodexManifest{}, "", err
	}
	defer cleanup()

	manifest, err := readCodexManifest(staging)
	if err != nil {
		return CodexManifest{}, "", err
	}
	binary := filepath.Join(staging, "bin", "codex")
	if err := verifyCodexBinary(binary, manifest); err != nil {
		return manifest, "", err
	}

	target := filepath.Join(o.Root, manifest.Short())
	if err := os.MkdirAll(o.Root, 0o755); err != nil {
		return manifest, "", err
	}
	if _, err := os.Stat(target); err == nil {
		logf("build %s is already installed", manifest.Short())
	} else {
		// Move the whole tree into place in one step. An extraction directly
		// into Root would be visible to a concurrent harness while incomplete.
		if err := os.Rename(staging, target); err != nil {
			if err := copyTree(staging, target); err != nil {
				return manifest, "", err
			}
		}
		logf("installed %s", target)
	}

	link := filepath.Join(o.Root, "current")
	if err := replaceSymlink(target, link); err != nil {
		return manifest, "", err
	}
	installed := filepath.Join(link, "bin", "codex")
	logf("current -> %s", target)
	return manifest, installed, nil
}

// stageCodex produces a package directory to install from.
func stageCodex(o CodexInstallOptions, logf func(string, ...any)) (string, func(), error) {
	if o.Source != "" {
		info, err := os.Stat(o.Source)
		if err != nil {
			return "", func() {}, err
		}
		if info.IsDir() {
			// Copy rather than install in place: the source is the operator's
			// build output, and moving it would take it away from them.
			dir, err := os.MkdirTemp("", "rca-codex-*")
			if err != nil {
				return "", func() {}, err
			}
			cleanup := func() { os.RemoveAll(dir) }
			staging := filepath.Join(dir, "pkg")
			if err := copyTree(o.Source, staging); err != nil {
				cleanup()
				return "", func() {}, err
			}
			return staging, cleanup, nil
		}
		body, err := os.ReadFile(o.Source)
		if err != nil {
			return "", func() {}, err
		}
		return extractCodex(body, logf)
	}

	base := o.ReleaseBase
	if base == "" {
		base = DefaultReleaseBase
	}
	archive := Local().CodexArchive()
	logf("downloading %s/%s", base, archive)
	body, err := get(base + "/" + archive)
	if err != nil {
		return "", func() {}, err
	}
	// The published digest sits beside the archive; a release that omits it is
	// refused rather than trusted.
	want, err := get(base + "/" + archive + ".sha256")
	if err != nil {
		return "", func() {}, fmt.Errorf("no published digest for %s: %w", archive, err)
	}
	fields := strings.Fields(string(want))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", func() {}, fmt.Errorf("unusable digest for %s", archive)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != fields[0] {
		return "", func() {}, fmt.Errorf("checksum mismatch for %s: got %s, want %s", archive, got, fields[0])
	}
	return extractCodex(body, logf)
}

// extractCodex unpacks a package tarball into a temporary directory.
func extractCodex(archive []byte, logf func(string, ...any)) (string, func(), error) {
	dir, err := os.MkdirTemp("", "rca-codex-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("package is not gzip: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		// The archive names its own paths, so it does not get to choose where
		// they land: anything escaping the staging directory is refused.
		target, err := safeJoin(dir, header.Name)
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				cleanup()
				return "", func() {}, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				cleanup()
				return "", func() {}, err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, header.FileInfo().Mode())
			if err != nil {
				cleanup()
				return "", func() {}, err
			}
			const maxEntry = 1 << 30
			if _, err := io.Copy(file, io.LimitReader(tr, maxEntry)); err != nil {
				file.Close()
				cleanup()
				return "", func() {}, err
			}
			if err := file.Close(); err != nil {
				cleanup()
				return "", func() {}, err
			}
		default:
			// Symlinks and devices in a package we are about to execute from
			// are not something to reproduce faithfully.
			cleanup()
			return "", func() {}, fmt.Errorf("package entry %q has unsupported type %c", header.Name, header.Typeflag)
		}
	}
	// The tarball wraps the package in one directory. macOS tar also emits an
	// AppleDouble "._name" sidecar per entry to carry extended attributes, so
	// "exactly one entry" is not a safe test — look for exactly one real
	// directory instead.
	entries, err := os.ReadDir(dir)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	var dirs []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "._") {
			continue
		}
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	if len(dirs) == 1 {
		return filepath.Join(dir, dirs[0]), cleanup, nil
	}
	return dir, cleanup, nil
}

// safeJoin refuses an archive entry that would escape the destination.
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(dir, name))
	if clean != dir && !strings.HasPrefix(clean, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("package entry %q escapes the staging directory", name)
	}
	return clean, nil
}

func readCodexManifest(dir string) (CodexManifest, error) {
	var manifest CodexManifest
	body, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return manifest, fmt.Errorf("package has no %s; build it with scripts/build-codex-package.sh: %w", manifestName, err)
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return manifest, fmt.Errorf("%s is not valid JSON: %w", manifestName, err)
	}
	return manifest, manifest.validate()
}

// verifyCodexBinary checks the binary against the hash its own manifest
// records. The digest on the archive proves it arrived as published; this
// proves the package's account of itself is internally consistent, which is
// what catches a hand-edited manifest or a swapped binary.
func verifyCodexBinary(path string, manifest CodexManifest) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("package has no bin/codex: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != manifest.BinarySHA256 {
		return fmt.Errorf("bin/codex does not match its manifest: got %s, want %s", got, manifest.BinarySHA256)
	}
	return nil
}

// replaceSymlink points link at target atomically.
func replaceSymlink(target, link string) error {
	staged := link + ".incoming"
	os.Remove(staged)
	if err := os.Symlink(target, staged); err != nil {
		return err
	}
	if err := os.Rename(staged, link); err != nil {
		os.Remove(staged)
		return err
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, info.Mode())
	})
}
