package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildCodexPackage writes a package directory the way
// scripts/build-codex-package.sh does, so the installer is tested against the
// shape it will actually meet.
func buildCodexPackage(t *testing.T, dir string, mutate func(*CodexManifest)) string {
	t.Helper()
	pkg := filepath.Join(dir, "codex-native")
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := []byte("#!/bin/sh\necho 'codex-cli 0.0.0'\n")
	if err := os.WriteFile(filepath.Join(pkg, "bin", "codex"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	manifest := CodexManifest{
		Upstream:         "https://github.com/openai/codex",
		UpstreamBaseline: "5ecb3afd1bf405149e2159bfda50093b0c1b5fab",
		BuiltFrom:        "276fefae10706a1fc4d0e515dc228f8c6a92eb46",
		PatchCount:       11,
		BinarySHA256:     hex.EncodeToString(sum[:]),
		BuiltOn:          "2026-09-09T22:10:55Z",
		License:          "Apache-2.0",
	}
	if mutate != nil {
		mutate(&manifest)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, manifestName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"LICENSE", "NOTICE", "CHANGES.md"} {
		if err := os.WriteFile(filepath.Join(pkg, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return pkg
}

func TestInstallCodexFromADirectory(t *testing.T) {
	dir := t.TempDir()
	pkg := buildCodexPackage(t, dir, nil)
	root := filepath.Join(dir, "installed")

	manifest, binary, err := InstallCodex(CodexInstallOptions{Source: pkg, Root: root, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.PatchCount != 11 {
		t.Errorf("manifest = %+v", manifest)
	}
	// current must be a symlink, so an upgrade is a pointer move rather than a
	// tree that is briefly half-replaced.
	link := filepath.Join(root, "current")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("current is not a symlink")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("the reported binary does not exist: %v", err)
	}
	// The source must survive: it is the operator's build output.
	if _, err := os.Stat(filepath.Join(pkg, "bin", "codex")); err != nil {
		t.Error("installing consumed the source package")
	}
	// The licence files have to travel with the binaries.
	for _, name := range []string{"LICENSE", "NOTICE", "CHANGES.md"} {
		if _, err := os.Stat(filepath.Join(root, manifest.Short(), name)); err != nil {
			t.Errorf("%s was not installed", name)
		}
	}
}

func TestInstallCodexIsIdempotentAndKeepsPreviousBuilds(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "installed")
	first := buildCodexPackage(t, filepath.Join(dir, "a"), nil)
	if _, _, err := InstallCodex(CodexInstallOptions{Source: first, Root: root, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	// Installing the same build again must not fail or duplicate.
	manifest, _, err := InstallCodex(CodexInstallOptions{Source: first, Root: root, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	firstShort := manifest.Short()

	second := buildCodexPackage(t, filepath.Join(dir, "b"), func(m *CodexManifest) {
		m.BuiltFrom = "aaaaaaaaaaaa1111111111111111111111111111"
	})
	next, _, err := InstallCodex(CodexInstallOptions{Source: second, Root: root, Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// The previous build stays reachable: if the new one is wrong, rolling
	// back must not mean rebuilding.
	if _, err := os.Stat(filepath.Join(root, firstShort, "bin", "codex")); err != nil {
		t.Error("the previous build was removed")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(resolved) != next.Short() {
		t.Errorf("current points at %s, want %s", filepath.Base(resolved), next.Short())
	}
}

// A package whose manifest does not describe its own binary is the case a
// transport checksum cannot catch.
func TestInstallCodexRefusesAMismatchedBinary(t *testing.T) {
	dir := t.TempDir()
	pkg := buildCodexPackage(t, dir, func(m *CodexManifest) {
		m.BinarySHA256 = strings.Repeat("a", 64)
	})
	root := filepath.Join(dir, "installed")
	if _, _, err := InstallCodex(CodexInstallOptions{Source: pkg, Root: root, Out: io.Discard}); err == nil {
		t.Fatal("a package whose binary does not match its manifest was installed")
	}
	if _, err := os.Stat(filepath.Join(root, "current")); err == nil {
		t.Error("current was pointed at a refused package")
	}
}

// Stock Codex fails the audited-native handshake, so installing it would look
// like an upgrade and behave like a removal.
func TestInstallCodexRefusesAnUnpatchedBuild(t *testing.T) {
	dir := t.TempDir()
	pkg := buildCodexPackage(t, dir, func(m *CodexManifest) { m.PatchCount = 0 })
	if _, _, err := InstallCodex(CodexInstallOptions{
		Source: pkg, Root: filepath.Join(dir, "installed"), Out: io.Discard,
	}); err == nil {
		t.Fatal("a package claiming no patches was installed")
	}
}

func TestInstallCodexRefusesAPackageWithoutAManifest(t *testing.T) {
	dir := t.TempDir()
	pkg := buildCodexPackage(t, dir, nil)
	if err := os.Remove(filepath.Join(pkg, manifestName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallCodex(CodexInstallOptions{
		Source: pkg, Root: filepath.Join(dir, "installed"), Out: io.Discard,
	}); err == nil {
		t.Fatal("a package with no manifest was installed")
	}
}

// The archive names its own paths, so it must not get to choose where they
// land.
func TestExtractCodexRefusesAnEscapingEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("owned")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escaped.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	zw.Close()

	if _, cleanup, err := extractCodex(buf.Bytes(), func(string, ...any) {}); err == nil {
		cleanup()
		t.Fatal("an entry escaping the staging directory was extracted")
	}
}

// A symlink inside a package we are about to execute from is not something to
// reproduce faithfully.
func TestExtractCodexRefusesASymlinkEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "bin/codex", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()

	if _, cleanup, err := extractCodex(buf.Bytes(), func(string, ...any) {}); err == nil {
		cleanup()
		t.Fatal("a symlink entry was extracted")
	}
}

func TestCodexArchiveNaming(t *testing.T) {
	if got := (Platform{"linux", "arm64"}).CodexArchive(); got != "codex-native_linux_arm64.tar.gz" {
		t.Errorf("CodexArchive() = %q", got)
	}
}
