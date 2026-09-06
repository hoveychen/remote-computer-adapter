package codexnative

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSessionPolicyAndReadiness(t *testing.T) {
	for _, scenario := range []string{"ready", "missing_state", "extra_mcp", "local_available", "registry_broken", "turn_failed"} {
		t.Run(scenario, func(t *testing.T) {
			clientRead, serverWrite := io.Pipe()
			serverRead, clientWrite := io.Pipe()
			defer clientRead.Close()
			defer serverWrite.Close()
			defer serverRead.Close()
			defer clientWrite.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			serverDone := make(chan error, 1)
			turns := make(chan bool, 1)
			go func() {
				dec := json.NewDecoder(serverRead)
				enc := json.NewEncoder(serverWrite)
				for {
					var q struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
						Params map[string]any  `json:"params"`
					}
					if e := dec.Decode(&q); e != nil {
						serverDone <- nil
						return
					}
					if len(q.ID) == 0 {
						continue
					}
					r := map[string]any{"id": q.ID, "jsonrpc": "2.0", "result": map[string]any{}}
					switch q.Method {
					case "initialize":
					case "environment/info":
						if q.Params["environmentId"] == "local" && scenario != "local_available" {
							delete(r, "result")
							msg := "unknown environment id `local`"
							if scenario == "registry_broken" {
								msg = "transport broken"
							}
							r["error"] = map[string]any{"code": -32600, "message": msg}
						}
					case "thread/start":
						env := q.Params["environments"].([]any)[0].(map[string]any)
						if q.Params["cwd"] != "/trusted/runtime/work" || env["environmentId"] != "third-party" || env["cwd"] != "/remote-only" {
							serverDone <- fmt.Errorf("wrong cwd binding: %v", q.Params)
							return
						}
						r["result"] = map[string]any{"thread": map[string]any{"id": "thread-test"}}
					case "mcpServerStatus/list":
						tools := map[string]any{}
						for _, c := range []string{"memory", "skills"} {
							for _, op := range []string{"list", "read", "put", "delete"} {
								name := c + "_" + op
								tools[name] = map[string]any{"name": name}
							}
						}
						if scenario == "missing_state" {
							delete(tools, "memory_put")
						}
						data := []any{map[string]any{"name": "rca_state", "tools": tools}}
						if scenario == "extra_mcp" {
							data = append(data, map[string]any{"name": "evil", "tools": map[string]any{}})
						}
						r["result"] = map[string]any{"data": data, "nextCursor": nil}
					case "turn/start":
						turns <- true
					default:
						serverDone <- fmt.Errorf("unexpected RPC %s", q.Method)
						return
					}
					if e := enc.Encode(r); e != nil {
						serverDone <- e
						return
					}
					if q.Method == "turn/start" {
						status := "completed"
						if scenario == "turn_failed" {
							status = "failed"
						}
						enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"status": status}}})
					}
				}
			}()
			e := driveSession(ctx, Config{RuntimeHome: "/trusted/runtime", RemoteCWD: "/remote-only"}, []string{"exec", "--json", "--", "test"}, clientWrite, clientRead)
			clientWrite.Close()
			if serverErr := <-serverDone; serverErr != nil {
				t.Fatal(serverErr)
			}
			if scenario == "ready" {
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("unsafe session accepted")
			}
			if scenario == "turn_failed" && !strings.Contains(e.Error(), "turn failed") {
				t.Fatal(e)
			}
			started := false
			select {
			case <-turns:
				started = true
			default:
			}
			if started != (scenario == "ready" || scenario == "turn_failed") {
				t.Fatal("model turn started before policy verification", scenario)
			}
		})
	}
}
