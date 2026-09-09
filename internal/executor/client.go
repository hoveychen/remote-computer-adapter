package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// Client speaks the executor protocol over one process's stdio. It is the only
// way the trusted side reaches the remote: there is no local backend to fall
// back to, so a dead transport surfaces as an error rather than as a quietly
// local file read.
type Client struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *jsonLines
	next uint64
	dead error
}

type jsonLines struct {
	dec *json.Decoder
}

// Dial launches the transport program and returns a client speaking to the
// executor on the far end. Program and args come from the trusted config, never
// from the model. The child inherits nothing: its environment is fixed here so
// harness credentials cannot reach the remote host.
func Dial(program string, args []string, stderr io.Writer) (*Client, error) {
	cmd := exec.Command(program, args...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		return nil, err
	}
	dec := json.NewDecoder(out)
	return &Client{cmd: cmd, in: in, out: &jsonLines{dec: dec}}, nil
}

// Close shuts the transport down and reaps the child.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil {
		return nil
	}
	c.in.Close()
	if c.cmd.Process != nil {
		syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
	}
	err := c.cmd.Wait()
	c.cmd = nil
	return err
}

// Call sends one request and returns its result. Requests are serialised: the
// protocol is one line in, one line out, so concurrent callers would otherwise
// interleave and read each other's replies.
func (c *Client) Call(req *Request, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead != nil {
		return c.dead
	}
	c.next++
	req.ID = c.next
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		return c.die(fmt.Errorf("executor transport closed: %w", err))
	}
	var resp Response
	if err := c.out.dec.Decode(&resp); err != nil {
		return c.die(fmt.Errorf("executor transport closed: %w", err))
	}
	if resp.ID != req.ID {
		return c.die(fmt.Errorf("executor replied to %d, expected %d", resp.ID, req.ID))
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(resp.Result, result)
}

// die records a transport failure. Every later call reports the same error
// instead of retrying: a half-open transport must not look like a fresh one.
func (c *Client) die(err error) error {
	if c.dead == nil {
		c.dead = err
	}
	return c.dead
}

// ServeStdio runs a server on this process's stdio. It is what the remote host
// invokes: `rca serve --root <dir>`.
func ServeStdio(ctx context.Context, root string) error {
	s, err := NewServer(root)
	if err != nil {
		return err
	}
	return s.Serve(ctx, os.Stdin, os.Stdout)
}
