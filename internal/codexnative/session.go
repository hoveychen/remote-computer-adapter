package codexnative

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// driveSession is a narrow app-server client, not a general RPC pass-through.
// Local bootstrap cwd and remote tool cwd are separate native API fields.
func driveSession(ctx context.Context, c Config, args []string, in io.Writer, out io.Reader) error {
	prompt := ""
	jsonOutput := false
	for i, a := range args {
		if a == "--json" {
			jsonOutput = true
		}
		if a == "--" && i+1 < len(args) {
			prompt = args[i+1]
			break
		}
	}
	if prompt == "" || prompt == "-" {
		b, e := io.ReadAll(io.LimitReader(os.Stdin, 4*1024*1024+1))
		if e != nil {
			return e
		}
		if len(b) > 4*1024*1024 {
			return errors.New("prompt exceeds 4 MiB")
		}
		prompt = string(b)
	}
	if prompt == "" {
		return errors.New("prompt is required")
	}
	messages := make(chan rpcMessage)
	readErr := make(chan error, 1)
	go func() {
		scan := bufio.NewScanner(out)
		scan.Buffer(make([]byte, 4096), 16*1024*1024)
		for scan.Scan() {
			var m rpcMessage
			if e := json.Unmarshal(scan.Bytes(), &m); e != nil {
				readErr <- e
				return
			}
			select {
			case messages <- m:
			case <-ctx.Done():
				return
			}
		}
		if e := scan.Err(); e != nil {
			readErr <- e
		} else {
			readErr <- io.ErrUnexpectedEOF
		}
	}()
	enc := json.NewEncoder(in)
	display := json.NewEncoder(os.Stdout)
	sequence := 0
	completed := false
	printedDelta := false
	var turnErr error
	handle := func(m rpcMessage) error {
		if len(m.ID) > 0 && m.Method != "" {
			// Approval and elicitation are unsupported in this never-approval prototype.
			return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "interactive server requests are disabled by RCA prototype"}})
		}
		if m.Method == "" {
			return nil
		}
		if jsonOutput {
			if e := display.Encode(map[string]any{"method": m.Method, "params": m.Params}); e != nil {
				return e
			}
		}
		if m.Method == "item/agentMessage/delta" && !jsonOutput {
			var p struct {
				Delta string `json:"delta"`
			}
			json.Unmarshal(m.Params, &p)
			fmt.Fprint(os.Stdout, p.Delta)
			printedDelta = true
		}
		if m.Method == "item/completed" && !jsonOutput && !printedDelta {
			var p struct {
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if e := json.Unmarshal(m.Params, &p); e != nil {
				return e
			}
			if p.Item.Type == "agentMessage" {
				fmt.Fprint(os.Stdout, p.Item.Text)
			}
		}
		if m.Method == "turn/completed" {
			var p struct {
				Turn struct {
					Status string          `json:"status"`
					Error  json.RawMessage `json:"error"`
				} `json:"turn"`
			}
			if e := json.Unmarshal(m.Params, &p); e != nil {
				return e
			}
			completed = true
			if p.Turn.Status != "completed" {
				turnErr = fmt.Errorf("turn %s: %s", p.Turn.Status, p.Turn.Error)
			}
		}
		return nil
	}
	next := func(ctx context.Context) (rpcMessage, error) {
		select {
		case m := <-messages:
			return m, nil
		case e := <-readErr:
			return rpcMessage{}, e
		case <-ctx.Done():
			return rpcMessage{}, ctx.Err()
		}
	}
	call := func(ctx context.Context, method string, params any) (json.RawMessage, error) {
		sequence++
		id := sequence
		if e := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); e != nil {
			return nil, e
		}
		for {
			m, e := next(ctx)
			if e != nil {
				return nil, e
			}
			if string(m.ID) == fmt.Sprint(id) && m.Method == "" {
				if len(m.Error) > 0 {
					return nil, fmt.Errorf("%s: %s", method, m.Error)
				}
				return m.Result, nil
			}
			if e := handle(m); e != nil {
				return nil, e
			}
		}
	}
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, e := call(startup, "initialize", map[string]any{"clientInfo": map[string]any{"name": "rca_native", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}); e != nil {
		return e
	}
	if e := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); e != nil {
		return e
	}
	// Validate both positive and negative registry behavior before a model turn.
	if _, e := call(startup, "environment/info", map[string]any{"environmentId": "third-party"}); e != nil {
		return e
	}
	if _, e := call(startup, "environment/info", map[string]any{"environmentId": "local"}); e == nil {
		return errors.New("local environment unexpectedly available")
	} else if !strings.Contains(e.Error(), "unknown environment id") {
		return fmt.Errorf("cannot verify local environment exclusion: %w", e)
	}
	raw, e := call(startup, "thread/start", map[string]any{
		"cwd":            filepath.Join(c.RuntimeHome, "work"),
		"environments":   []any{map[string]any{"environmentId": "third-party", "cwd": c.RemoteCWD}},
		"approvalPolicy": "never", "sandbox": "danger-full-access",
	})
	if e != nil {
		return e
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if e = json.Unmarshal(raw, &thread); e != nil {
		return e
	}
	if thread.Thread.ID == "" {
		return errors.New("missing thread id")
	}
	raw, e = call(startup, "mcpServerStatus/list", map[string]any{"threadId": thread.Thread.ID, "limit": 100})
	if e != nil {
		return e
	}
	var inventory struct {
		Data []struct {
			Name  string `json:"name"`
			Tools map[string]struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	if e = json.Unmarshal(raw, &inventory); e != nil {
		return e
	}
	if len(inventory.Data) != 1 || inventory.Data[0].Name != "rca_state" || inventory.NextCursor != nil {
		return errors.New("unexpected MCP inventory; refusing model turn")
	}
	names := map[string]bool{}
	for _, tool := range inventory.Data[0].Tools {
		names[tool.Name] = true
	}
	for _, collection := range []string{"memory", "skills"} {
		for _, op := range []string{"list", "read", "put", "delete"} {
			if !names[collection+"_"+op] {
				return errors.New("trusted state MCP not ready; refusing model turn")
			}
		}
	}
	if len(names) != 8 {
		return errors.New("unexpected trusted state tools")
	}
	if _, e = call(startup, "turn/start", map[string]any{"threadId": thread.Thread.ID, "input": []any{map[string]any{"type": "text", "text": prompt, "text_elements": []any{}}}}); e != nil {
		return e
	}
	for !completed {
		m, e := next(ctx)
		if e != nil {
			return e
		}
		if e = handle(m); e != nil {
			return e
		}
	}
	if !jsonOutput {
		fmt.Fprintln(os.Stdout)
	}
	return turnErr
}
