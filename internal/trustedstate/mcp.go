package trustedstate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

func strictJSON(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
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
func schemas() []any {
	out := []any{}
	for _, collection := range []string{"memory", "skills"} {
		for _, op := range []string{"list", "read", "put", "delete"} {
			props := map[string]any{}
			required := []string{}
			if op != "list" {
				props["id"] = map[string]any{"type": "string", "pattern": logicalID.String()}
				required = append(required, "id")
			}
			if op == "put" {
				props["content"] = map[string]any{"type": "string", "maxLength": MaxContent}
				required = append(required, "content")
			}
			if op == "put" || op == "delete" {
				props["expected_revision"] = map[string]any{"type": "integer", "minimum": 0}
				props["request_id"] = map[string]any{"type": "string", "pattern": logicalID.String()}
				required = append(required, "expected_revision", "request_id")
			}
			out = append(out, map[string]any{"name": collection + "_" + op, "description": fmt.Sprintf("%s trusted %s objects. IDs are logical, never paths. Mutations require CAS revision and an idempotent request_id; list includes deletion tombstones.", op, collection), "inputSchema": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}})
		}
	}
	return out
}
func (s *Store) call(name string, args json.RawMessage) (any, error) {
	parts := strings.Split(name, "_")
	if len(parts) != 2 {
		return nil, errors.New("unknown tool")
	}
	c, op := parts[0], parts[1]
	if c != "memory" && c != "skills" {
		return nil, errors.New("unknown tool")
	}
	// Field-presence validation matters: a missing expected_revision is not zero.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return nil, errors.New("arguments must be an object")
	}
	required := []string{}
	allowed := map[string]bool{}
	switch op {
	case "list":
	case "read":
		required = []string{"id"}
	case "put":
		required = []string{"id", "content", "expected_revision", "request_id"}
	case "delete":
		required = []string{"id", "expected_revision", "request_id"}
	default:
		return nil, errors.New("unknown tool")
	}
	for _, k := range required {
		allowed[k] = true
		v, ok := fields[k]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, fmt.Errorf("missing %s", k)
		}
	}
	for k := range fields {
		if !allowed[k] {
			return nil, fmt.Errorf("unknown argument %s", k)
		}
	}
	var q Request
	if err := strictJSON(args, &q); err != nil {
		return nil, err
	}
	switch op {
	case "list":
		return s.List(c)
	case "read":
		return s.Read(c, q.ID)
	default:
		r, err := s.Mutate(c, op, q)
		if err == nil && r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, err
	}
}

// Serve implements newline-delimited MCP stdio. No paths, resources, shell,
// sampling, or model-accessible audit mutation endpoints are exposed.
func Serve(in io.Reader, out io.Writer, s *Store) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), 8*MaxContent)
	enc := json.NewEncoder(out)
	initialized := false
	for scan.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		err := strictJSON(scan.Bytes(), &req)
		reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		rpcErr := func(code int, msg string) { reply["error"] = map[string]any{"code": code, "message": msg} }
		if err != nil {
			reply["id"] = nil
			rpcErr(-32700, "invalid JSON-RPC request")
		} else if req.JSONRPC != "2.0" {
			rpcErr(-32600, "jsonrpc must be 2.0")
		} else if len(req.ID) == 0 {
			// Notifications may never mutate state.
			continue
		} else {
			switch req.Method {
			case "initialize":
				var p struct {
					ProtocolVersion string          `json:"protocolVersion"`
					Capabilities    json.RawMessage `json:"capabilities"`
					ClientInfo      json.RawMessage `json:"clientInfo"`
				}
				if err := strictJSON(req.Params, &p); err != nil {
					rpcErr(-32602, "invalid initialize params")
					break
				}
				version := p.ProtocolVersion
				switch version {
				case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
				default:
					version = "2024-11-05"
				}
				initialized = true
				reply["result"] = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "rca-trusted-state", "version": "0.1.0"}}
			case "ping":
				reply["result"] = map[string]any{}
			case "tools/list":
				if !initialized {
					rpcErr(-32600, "initialize first")
				} else {
					reply["result"] = map[string]any{"tools": schemas()}
				}
			case "tools/call":
				if !initialized {
					rpcErr(-32600, "initialize first")
					break
				}
				var p struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
					Meta      json.RawMessage `json:"_meta,omitempty"`
				}
				if err := strictJSON(req.Params, &p); err != nil {
					rpcErr(-32602, "invalid tool params")
					break
				}
				result, err := s.call(p.Name, p.Arguments)
				var payload []byte
				if err != nil {
					payload, _ = json.Marshal(map[string]any{"error": err.Error(), "result": result})
				} else {
					payload, _ = json.Marshal(result)
				}
				reply["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(payload)}}, "isError": err != nil}
			default:
				rpcErr(-32601, "method not found")
			}
		}
		if err := enc.Encode(reply); err != nil {
			return err
		}
	}
	return scan.Err()
}
