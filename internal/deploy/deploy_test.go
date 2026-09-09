package deploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseUnameMapsRealOutput(t *testing.T) {
	for _, c := range []struct {
		s, m string
		want Platform
	}{
		{"Darwin", "arm64", Platform{"darwin", "arm64"}},
		{"Darwin", "x86_64", Platform{"darwin", "amd64"}},
		{"Linux", "x86_64", Platform{"linux", "amd64"}},
		{"Linux", "aarch64", Platform{"linux", "arm64"}},
		// uname output arrives with the newline still attached.
		{"Linux\n", "  x86_64  ", Platform{"linux", "amd64"}},
	} {
		got, err := ParseUname(c.s, c.m)
		if err != nil {
			t.Errorf("ParseUname(%q, %q): %v", c.s, c.m, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseUname(%q, %q) = %v, want %v", c.s, c.m, got, c.want)
		}
	}
}

// An unrecognised platform must fail where the operator can read the values,
// not resolve to something plausible and install a binary that cannot run.
func TestParseUnameRejectsUnsupported(t *testing.T) {
	for _, c := range [][2]string{
		{"FreeBSD", "amd64"},
		{"Linux", "riscv64"},
		{"MINGW64_NT-10.0", "x86_64"},
		{"", ""},
	} {
		if got, err := ParseUname(c[0], c[1]); err == nil {
			t.Errorf("ParseUname(%q, %q) = %v, want an error", c[0], c[1], got)
		}
	}
}

func TestArchiveNamesHaveNoVersion(t *testing.T) {
	// Release URLs use releases/latest/download, so the names must be stable.
	if got := (Platform{"linux", "amd64"}).Archive(); got != "rca_linux_amd64.tar.gz" {
		t.Errorf("Archive() = %q", got)
	}
}

// buildArchive makes a release tarball with the given rca payload.
func buildArchive(t *testing.T, payload []byte, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releaseServer serves a checksums.txt and archives, so the download path is
// exercised end to end rather than mocked away.
func releaseServer(t *testing.T, archives map[string][]byte, checksums string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, checksums)
	})
	for name, body := range archives {
		body := body
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Write(body)
		})
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func TestDownloadVerifiesAndExtracts(t *testing.T) {
	payload := []byte("#!/bin/sh\necho rca\n")
	archive := buildArchive(t, payload, "rca")
	base := releaseServer(t, map[string][]byte{"rca_linux_amd64.tar.gz": archive},
		sum(archive)+"  rca_linux_amd64.tar.gz\n")

	path, cleanup, err := obtain(Options{ReleaseBase: base}, Platform{"linux", "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("extracted %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Error("the extracted binary is not executable")
	}
}

// A tampered archive must not be unpacked at all — not unpacked and then
// discarded, which would still have written attacker bytes to disk.
func TestDownloadRefusesAChecksumMismatch(t *testing.T) {
	archive := buildArchive(t, []byte("payload"), "rca")
	base := releaseServer(t, map[string][]byte{"rca_linux_amd64.tar.gz": archive},
		strings.Repeat("0", 64)+"  rca_linux_amd64.tar.gz\n")

	_, _, err := obtain(Options{ReleaseBase: base}, Platform{"linux", "amd64"})
	if err == nil {
		t.Fatal("a mismatched archive was accepted")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v, want a checksum mismatch", err)
	}
}

func TestDownloadRefusesAMissingChecksumEntry(t *testing.T) {
	archive := buildArchive(t, []byte("payload"), "rca")
	base := releaseServer(t, map[string][]byte{"rca_linux_amd64.tar.gz": archive},
		sum(archive)+"  rca_darwin_arm64.tar.gz\n")

	if _, _, err := obtain(Options{ReleaseBase: base}, Platform{"linux", "amd64"}); err == nil {
		t.Fatal("an archive with no published digest was accepted")
	}
}

func TestExtractRejectsAnArchiveWithoutRCA(t *testing.T) {
	archive := buildArchive(t, []byte("payload"), "README")
	if _, err := extractRCA(archive); err == nil {
		t.Fatal("an archive with no rca entry was accepted")
	}
	if _, err := extractRCA([]byte("not gzip at all")); err == nil {
		t.Fatal("a non-gzip body was accepted")
	}
}

// A local binary is the operator's assertion, so it is used as-is — but a path
// that does not exist must fail before anything is copied.
func TestObtainUsesALocalBinary(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "rca")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	path, cleanup, err := obtain(Options{Binary: file.Name()}, Platform{"linux", "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if path != file.Name() {
		t.Errorf("path = %q, want the local binary", path)
	}
	if _, _, err := obtain(Options{Binary: file.Name() + "-missing"}, Platform{"linux", "amd64"}); err == nil {
		t.Error("a missing local binary was accepted")
	}
}

func TestShellQuoteSurvivesAwkwardPaths(t *testing.T) {
	for _, path := range []string{
		"/work/my project",
		"/work/it's here",
		"/work/;rm -rf /",
		"/work/$(whoami)",
	} {
		quoted := shellQuote(path)
		out, err := runLocalShell("printf %s " + quoted)
		if err != nil {
			t.Fatalf("%q: %v", path, err)
		}
		if out != path {
			t.Errorf("%q round-tripped as %q", path, out)
		}
	}
}

func TestRunRejectsAnEmptyTarget(t *testing.T) {
	if _, err := Run(Options{Target: "  "}); err == nil {
		t.Fatal("an empty ssh target was accepted")
	}
}

// runLocalShell runs a command through /bin/sh, so the quoting test checks the
// same interpretation the remote shell will apply.
func runLocalShell(command string) (string, error) {
	out, err := exec.Command("/bin/sh", "-c", command).Output()
	return string(out), err
}
