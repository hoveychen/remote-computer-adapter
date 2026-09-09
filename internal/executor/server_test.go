package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	// EvalSymlinks so the fixture root matches what NewServer resolves; on
	// macOS t.TempDir() hands back a /var symlink into /private/var.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func call(t *testing.T, s *Server, req *Request) (map[string]any, error) {
	t.Helper()
	result, err := s.Handle(context.Background(), req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func TestReadWriteRoundTrip(t *testing.T) {
	s, root := newTestServer(t)
	if _, err := call(t, s, &Request{Op: OpWrite, Path: "sub/note.txt", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "sub", "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != "hello" {
		t.Fatalf("on disk = %q, want %q", onDisk, "hello")
	}
	got, err := call(t, s, &Request{Op: OpRead, Path: "sub/note.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got["content"] != "hello" {
		t.Fatalf("read back %q", got["content"])
	}
	if got["path"] != filepath.Join("sub", "note.txt") {
		t.Fatalf("path = %q, want a root-relative path", got["path"])
	}
}

// A failed write must not destroy the previous content: the implementation
// renames a completed temp file over the target rather than truncating it.
func TestWriteOverLimitLeavesOldContent(t *testing.T) {
	s, root := newTestServer(t)
	target := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, s, &Request{Op: OpWrite, Path: "keep.txt",
		Content: strings.Repeat("x", MaxContent+1)})
	if err == nil {
		t.Fatal("oversized write was accepted")
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "original" {
		t.Fatalf("content = %q, want the write to have left it alone", body)
	}
}

func TestPathEscapesRejected(t *testing.T) {
	s, _ := newTestServer(t)
	for _, path := range []string{"../outside.txt", "/etc/passwd", "sub/../../outside.txt"} {
		if _, err := call(t, s, &Request{Op: OpRead, Path: path}); err == nil {
			t.Errorf("%q was accepted", path)
		}
	}
}

// A symlink planted inside the root is the interesting case: the path is
// textually in-root, so only symlink resolution catches it.
func TestSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, root := newTestServer(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, &Request{Op: OpRead, Path: "link.txt"}); err == nil {
		t.Fatal("read through an escaping symlink was allowed")
	}
	// Writing through it must fail too, or the link becomes a write primitive.
	if _, err := call(t, s, &Request{Op: OpWrite, Path: "link.txt", Content: "x"}); err == nil {
		t.Fatal("write through an escaping symlink was allowed")
	}
	if body, _ := os.ReadFile(outside); string(body) != "SECRET" {
		t.Fatal("the outside file was modified")
	}
}

// A symlinked directory is the same escape one level up.
func TestSymlinkedDirEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, root := newTestServer(t)
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(root, "door")); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, &Request{Op: OpWrite, Path: "door/new.txt", Content: "x"}); err == nil {
		t.Fatal("write through an escaping directory symlink was allowed")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); err == nil {
		t.Fatal("the file landed outside the root")
	}
}

func TestListMarksEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(root, "away")); err != nil {
		t.Fatal(err)
	}
	got, err := call(t, s, &Request{Op: OpList})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, raw := range got["entries"].([]any) {
		e := raw.(map[string]any)
		if e["name"] == "away" {
			found = true
			if e["alien"] != true {
				t.Error("escaping symlink was not marked alien")
			}
		}
	}
	if !found {
		t.Error("escaping symlink was omitted from the listing instead of marked")
	}
}

func TestSearchFindsAndBoundsResults(t *testing.T) {
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := call(t, s, &Request{Op: OpSearch, Pattern: "^beta$"})
	if err != nil {
		t.Fatal(err)
	}
	matches := got["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1 (.git must be skipped)", len(matches))
	}
	m := matches[0].(map[string]any)
	if m["path"] != "a.txt" || m["line"].(float64) != 2 {
		t.Fatalf("match = %v", m)
	}
}

func TestSearchRejectsBadPattern(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := call(t, s, &Request{Op: OpSearch, Pattern: "("}); err == nil {
		t.Fatal("an invalid regexp was accepted")
	}
}

func TestExecRunsInRootWithFixedEnv(t *testing.T) {
	s, root := newTestServer(t)
	result, err := s.Handle(context.Background(), &Request{
		Op: OpExec, Argv: []string{"/bin/sh", "-c", "pwd; printf %s \"$RCA_LEAK\""}})
	if err != nil {
		t.Fatal(err)
	}
	r := result.(ExecResult)
	if r.ExitCode != 0 {
		t.Fatalf("exit %d, stderr %q", r.ExitCode, r.Stderr)
	}
	if strings.TrimSpace(r.Stdout) != root {
		t.Fatalf("cwd = %q, want %q", strings.TrimSpace(r.Stdout), root)
	}
}

// The executor must not forward its own environment: whatever started the ssh
// session is not something the trusted side vouched for.
func TestExecDoesNotInheritEnvironment(t *testing.T) {
	s, _ := newTestServer(t)
	t.Setenv("RCA_SECRET_CANARY", "leaked")
	result, err := s.Handle(context.Background(), &Request{
		Op: OpExec, Argv: []string{"/bin/sh", "-c", "printf %s \"$RCA_SECRET_CANARY\""}})
	if err != nil {
		t.Fatal(err)
	}
	if out := result.(ExecResult).Stdout; out != "" {
		t.Fatalf("child saw the parent's environment: %q", out)
	}
}

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	s, _ := newTestServer(t)
	start := time.Now()
	result, err := s.Handle(context.Background(), &Request{
		Op: OpExec, Argv: []string{"/bin/sh", "-c", "sleep 30"}, TimeoutMS: 300})
	if err != nil {
		t.Fatal(err)
	}
	r := result.(ExecResult)
	if !r.TimedOut {
		t.Fatal("timed_out was not reported")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %s; the timeout did not kill the child", elapsed)
	}
}

func TestExecTruncatesRunawayOutput(t *testing.T) {
	s, _ := newTestServer(t)
	result, err := s.Handle(context.Background(), &Request{
		Op:   OpExec,
		Argv: []string{"/bin/sh", "-c", "yes rca | head -c 20000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := result.(ExecResult)
	if !r.Truncated {
		t.Fatal("truncated was not reported")
	}
	if len(r.Stdout) > MaxContent {
		t.Fatalf("kept %d bytes, over the %d limit", len(r.Stdout), MaxContent)
	}
}

func TestExecCwdCannotEscape(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := s.Handle(context.Background(), &Request{
		Op: OpExec, Argv: []string{"/bin/echo", "hi"}, Cwd: "../"}); err == nil {
		t.Fatal("an escaping cwd was accepted")
	}
}

func TestUnknownOpRejected(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := s.Handle(context.Background(), &Request{Op: "fs.delete"}); err == nil {
		t.Fatal("an unknown op was accepted")
	}
}

func TestNewServerRejectsRelativeAndFileRoots(t *testing.T) {
	if _, err := NewServer("relative/path"); err == nil {
		t.Error("a relative root was accepted")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(file); err == nil {
		t.Error("a file root was accepted")
	}
}
