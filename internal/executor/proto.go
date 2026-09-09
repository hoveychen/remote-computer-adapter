// Package executor is the untrusted remote sidecar. It runs on the sandbox
// host and performs the file and subprocess operations that the trusted
// harness's MCP tools forward to it.
//
// The wire format is newline-delimited JSON over stdio, so the transport is
// whatever program the trusted side execs — typically ssh. That mirrors
// `codex exec-server --listen stdio`: the executor never listens on a socket
// and never authenticates, because reaching its stdin already means you are
// the trusted side.
//
// Operations are at MCP-tool granularity (read a file, run a command), not
// syscall granularity. Nothing here is a general RPC surface: every path is
// confined to one root, and the request set is closed.
package executor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// MaxContent caps a single file body or captured output stream.
const MaxContent = 8 << 20 // 8 MiB

// Op names. The set is closed; an unknown op is an error, never a fallthrough.
const (
	OpRead    = "fs.read"
	OpWrite   = "fs.write"
	OpList    = "fs.list"
	OpSearch  = "fs.search"
	OpExec    = "exec.run"
	OpVersion = "version"
)

// Request is one operation. Fields not used by an op must be absent; the
// decoder rejects unknown fields so a typo never reads as a default.
type Request struct {
	ID   uint64 `json:"id"`
	Op   string `json:"op"`
	Path string `json:"path,omitempty"`
	// Content is the new file body for fs.write.
	Content string `json:"content,omitempty"`
	// Pattern is the regexp for fs.search.
	Pattern string `json:"pattern,omitempty"`
	// Argv is the command for exec.run, argv[0] included.
	Argv []string `json:"argv,omitempty"`
	// Cwd is relative to the root; empty means the root itself.
	Cwd string `json:"cwd,omitempty"`
	// TimeoutMS bounds exec.run. Zero takes the server default.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// Response carries exactly one of Result or Error.
type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Entry is one directory member in a fs.list result.
type Entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
	Alien bool   `json:"alien,omitempty"` // symlink pointing outside the root
}

// Match is one hit in a fs.search result.
type Match struct {
	Path string `json:"path"` // relative to the root
	Line int    `json:"line"`
	Text string `json:"text"`
}

// ExecResult reports a finished subprocess. Output is captured, not streamed:
// an MCP tool call is request/response, so there is no channel to stream on.
type ExecResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// decodeStrict rejects unknown fields and trailing data.
func decodeStrict(line []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

// scanner returns a line scanner sized for the largest legal request: a
// fs.write body plus its JSON envelope.
func scanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 2*MaxContent)
	return s
}
