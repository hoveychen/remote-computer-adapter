package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// DefaultTimeout bounds exec.run when the request does not.
const DefaultTimeout = 120 * time.Second

// MaxTimeout is the ceiling a request may ask for.
const MaxTimeout = 30 * time.Minute

// Server answers requests against one root directory.
type Server struct {
	root string
	// Now and runner exist so tests can drive the clock and the subprocess
	// layer without spawning real processes.
	timeout time.Duration
}

// NewServer resolves root and returns a server confined to it. The resolved
// path is the one every request is checked against, so a symlinked root is
// pinned to its target at startup rather than re-resolved per request.
func NewServer(root string) (*Server, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("root must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("root must be a directory")
	}
	return &Server{root: resolved, timeout: DefaultTimeout}, nil
}

// Root reports the resolved root.
func (s *Server) Root() string { return s.root }

// resolve maps a request path to an absolute path inside the root.
//
// Relative paths are joined onto the root; absolute paths must already be
// inside it. Every existing component is symlink-resolved before the check, so
// a link planted inside the root cannot point out of it. A path that does not
// exist yet is resolved through its nearest existing ancestor, which is what
// makes fs.write to a new file safe.
func (s *Server) resolve(p string) (string, error) {
	return s.resolveDepth(p, 0)
}

// maxLinkDepth bounds symlink chasing so a link cycle cannot spin here.
const maxLinkDepth = 16

func (s *Server) resolveDepth(p string, depth int) (string, error) {
	if p == "" {
		return s.root, nil
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains NUL")
	}
	if depth > maxLinkDepth {
		return "", errors.New("too many levels of symbolic links")
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(s.root, p)
	}
	abs = filepath.Clean(abs)

	// A dangling symlink is the case EvalSymlinks cannot help with: its target
	// does not exist yet, so the walk below would resolve the *parent* and
	// re-append the link's own name, silently dropping the indirection. The
	// link would then read as in-root, and once something creates the target
	// the read escapes. Chase it explicitly instead.
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(abs), target)
		}
		return s.resolveDepth(target, depth+1)
	}

	// Walk up to the nearest existing ancestor, resolve that, then re-append
	// the missing tail. EvalSymlinks fails outright on a missing leaf.
	rest := ""
	probe := abs
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			abs = filepath.Join(resolved, rest)
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", errors.New("path escapes the root")
		}
		rest = filepath.Join(filepath.Base(probe), rest)
		probe = parent
	}
	if abs != s.root && !strings.HasPrefix(abs, s.root+string(filepath.Separator)) {
		return "", errors.New("path escapes the root")
	}
	return abs, nil
}

// rel reports an in-root path relative to the root, for results the trusted
// side shows to the model.
func (s *Server) rel(abs string) string {
	r, err := filepath.Rel(s.root, abs)
	if err != nil {
		return abs
	}
	return r
}

// Serve reads newline-delimited requests until in is exhausted. A malformed
// line is answered with an error and the stream continues; only an unwritable
// output ends the loop.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scan := scanner(in)
	enc := json.NewEncoder(out)
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 {
			continue
		}
		var req Request
		resp := Response{}
		if err := decodeStrict(line, &req); err != nil {
			resp.Error = "invalid request: " + err.Error()
		} else {
			resp.ID = req.ID
			result, err := s.Handle(ctx, &req)
			if err != nil {
				resp.Error = err.Error()
			} else if resp.Result, err = json.Marshal(result); err != nil {
				resp.Result, resp.Error = nil, err.Error()
			}
		}
		if err := enc.Encode(&resp); err != nil {
			return err
		}
	}
	return scan.Err()
}

// Handle runs one request. Every op validates its own inputs; an op never
// falls back to another op's behaviour when a field is missing.
func (s *Server) Handle(ctx context.Context, req *Request) (any, error) {
	switch req.Op {
	case OpVersion:
		return map[string]any{"protocol": 1, "root": s.root}, nil
	case OpRead:
		return s.read(req)
	case OpWrite:
		return s.write(req)
	case OpList:
		return s.list(req)
	case OpSearch:
		return s.search(req)
	case OpExec:
		return s.run(ctx, req)
	default:
		return nil, fmt.Errorf("unknown op %q", req.Op)
	}
}

func (s *Server) read(req *Request) (any, error) {
	abs, err := s.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("path is a directory")
	}
	if info.Size() > MaxContent {
		return nil, fmt.Errorf("file is %d bytes, over the %d limit", info.Size(), MaxContent)
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": s.rel(abs), "content": string(body), "size": len(body)}, nil
}

func (s *Server) write(req *Request) (any, error) {
	if len(req.Content) > MaxContent {
		return nil, fmt.Errorf("content is %d bytes, over the %d limit", len(req.Content), MaxContent)
	}
	abs, err := s.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	// Write to a sibling temp file and rename, so a reader never sees a torn
	// file and a failed write leaves the old content intact.
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".rca-write-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(req.Content); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return nil, err
	}
	return map[string]any{"path": s.rel(abs), "size": len(req.Content)}, nil
}

func (s *Server) list(req *Request) (any, error) {
	abs, err := s.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		e := Entry{Name: item.Name(), Dir: item.IsDir()}
		if info, err := item.Info(); err == nil {
			e.Size, e.Mode = info.Size(), info.Mode().String()
			if info.Mode()&os.ModeSymlink != 0 {
				// Report, rather than hide, a link the trusted side must not
				// follow: silently dropping it would misdescribe the directory.
				if _, err := s.resolve(filepath.Join(abs, item.Name())); err != nil {
					e.Alien = true
				}
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return map[string]any{"path": s.rel(abs), "entries": entries}, nil
}

// maxMatches caps a search result so one broad pattern cannot flood the
// trusted side's model context.
const maxMatches = 500

func (s *Server) search(req *Request) (any, error) {
	if req.Pattern == "" {
		return nil, errors.New("search requires a pattern")
	}
	re, err := regexp.Compile(req.Pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	root, err := s.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	matches := []Match{}
	truncated := false
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || len(matches) >= maxMatches {
			if len(matches) >= maxMatches {
				truncated = true
				return filepath.SkipAll
			}
			return nil // an unreadable entry is skipped, not fatal
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > MaxContent {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(body, 0) >= 0 {
			return nil // unreadable or binary
		}
		for i, line := range strings.Split(string(body), "\n") {
			if len(matches) >= maxMatches {
				truncated = true
				return filepath.SkipAll
			}
			if re.MatchString(line) {
				if len(line) > 500 {
					line = line[:500]
				}
				matches = append(matches, Match{Path: s.rel(path), Line: i + 1, Text: line})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"matches": matches, "truncated": truncated}, nil
}

func (s *Server) run(ctx context.Context, req *Request) (any, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("exec requires argv")
	}
	cwd, err := s.resolve(req.Cwd)
	if err != nil {
		return nil, err
	}
	timeout := s.timeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = cwd
	// The executor's own environment is not the harness's, but it is also not
	// something the trusted side vouched for; pass a fixed minimal one so a
	// command's behaviour does not depend on how the ssh session was started.
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + cwd,
		"PWD=" + cwd,
		"LANG=C.UTF-8",
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = MaxContent, MaxContent
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Own process group so a timeout kills the whole tree, not just the leader.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	result := ExecResult{}
	select {
	case err = <-done:
	case <-ctx.Done():
		result.TimedOut = true
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	result.ExitCode = cmd.ProcessState.ExitCode()
	if err != nil && cmd.ProcessState == nil {
		return nil, err
	}
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.Truncated = stdout.truncated || stderr.truncated
	return result, nil
}

// limitedBuffer keeps the first limit bytes and records that it dropped the
// rest, so a runaway command cannot exhaust memory here or the model's context
// on the trusted side.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			return b.buf.Write(p)
		}
		b.buf.Write(p[:room])
	}
	b.truncated = true
	return len(p), nil // report a full write: the command must not see EPIPE
}

func (b *limitedBuffer) String() string { return b.buf.String() }
