package executor

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A symlink whose target does not exist yet is the case plain symlink
// resolution misses: the leaf cannot be resolved, so a naive implementation
// resolves the parent and re-appends the link's own name, which reads as
// in-root. The escape lands later, when something creates the target.
func TestDanglingSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, root := newTestServer(t)
	outsideDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outsideDir, "not-yet-there.txt")
	if err := os.Symlink(target, filepath.Join(root, "trap")); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, &Request{Op: OpWrite, Path: "trap", Content: "x"}); err == nil {
		t.Fatal("write through a dangling escaping symlink was allowed")
	}
	// Now the target exists; the read must still be refused.
	if err := os.WriteFile(target, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, &Request{Op: OpRead, Path: "trap"}); err == nil {
		t.Fatal("read through a now-live escaping symlink was allowed")
	}
}

func TestSymlinkLoopTerminates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	s, root := newTestServer(t)
	if err := os.Symlink(filepath.Join(root, "b"), filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, s, &Request{Op: OpRead, Path: "a"}); err == nil {
		t.Fatal("a symlink loop resolved successfully")
	}
}

// Serve must answer a malformed line and keep going: one bad request from the
// trusted side should not take the executor down mid-session.
func TestServeSurvivesMalformedRequest(t *testing.T) {
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(strings.Join([]string{
		`{"id":1,"op":"version"}`,
		`not json at all`,
		`{"id":2,"op":"fs.read","path":"f.txt","bogus_field":1}`,
		`{"id":3,"op":"fs.read","path":"f.txt"}`,
	}, "\n") + "\n")
	out := &strings.Builder{}
	if err := s.Serve(context.Background(), in, out); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(out.String()))
	var got []Response
	for {
		var r Response
		if err := dec.Decode(&r); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 4 {
		t.Fatalf("got %d responses, want 4", len(got))
	}
	if got[0].Error != "" {
		t.Errorf("version failed: %s", got[0].Error)
	}
	if got[1].Error == "" {
		t.Error("malformed JSON was accepted")
	}
	if got[2].Error == "" {
		t.Error("an unknown field was accepted")
	}
	if got[3].Error != "" || !strings.Contains(string(got[3].Result), "body") {
		t.Errorf("the request after the bad ones failed: %+v", got[3])
	}
}

// The client and server together, over a real pipe pair.
func TestClientServerRoundTrip(t *testing.T) {
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	srvIn, cliOut := io.Pipe()
	cliIn, srvOut := io.Pipe()
	go func() {
		s.Serve(context.Background(), srvIn, srvOut)
		srvOut.Close()
	}()
	c := &Client{in: cliOut, out: &jsonLines{dec: json.NewDecoder(cliIn)}}

	var read struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Size    int    `json:"size"`
	}
	if err := c.Call(&Request{Op: OpRead, Path: "hello.txt"}, &read); err != nil {
		t.Fatal(err)
	}
	if read.Content != "world" {
		t.Fatalf("content = %q", read.Content)
	}
	// An error from the far side must surface as an error, not as empty data.
	if err := c.Call(&Request{Op: OpRead, Path: "../escape"}, &read); err == nil {
		t.Fatal("an escaping path came back without an error")
	}
}

// Once the transport dies, every later call must keep reporting it. A
// half-open transport that looks fresh is how a caller ends up retrying a
// side-effecting command against nothing.
func TestClientStaysDeadAfterTransportFailure(t *testing.T) {
	srvIn, cliOut := io.Pipe()
	cliIn, srvOut := io.Pipe()
	srvIn.Close()
	srvOut.Close()
	c := &Client{in: cliOut, out: &jsonLines{dec: json.NewDecoder(cliIn)}}

	first := c.Call(&Request{Op: OpVersion}, nil)
	if first == nil {
		t.Fatal("a call on a closed transport succeeded")
	}
	second := c.Call(&Request{Op: OpVersion}, nil)
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second call reported %v, want the recorded %v", second, first)
	}
}
