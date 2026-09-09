// Package toolgateway is the only tool surface the Claude harness is offered.
//
// It composes two backends that must never be confusable. General file and
// command work goes to the untrusted remote executor: those tools take paths
// relative to the executor's root and cannot name a host path, because there is
// no host backend behind them to name. Memory and skills go to the trusted
// state service: those tools take logical IDs and never paths, so the model
// cannot reach trusted state by describing a file.
//
// The separation is the point. A single "write a file" tool that chose its
// backend from the path would put that choice in the model's hands.
package toolgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hoveychen/remote-computer-adapter/internal/executor"
	"github.com/hoveychen/remote-computer-adapter/internal/trustedstate"
)

// Remote is the subset of the executor client the gateway uses. It exists so
// tests can drive the gateway without a transport, not so a second backend can
// be substituted at runtime: the concrete client is chosen at construction by
// the trusted side.
type Remote interface {
	Call(req *executor.Request, result any) error
}

// Gateway implements trustedstate.ToolSet.
type Gateway struct {
	state  *trustedstate.Store
	remote Remote
}

// New binds the gateway to its two backends.
func New(state *trustedstate.Store, remote Remote) *Gateway {
	return &Gateway{state: state, remote: remote}
}

const (
	toolRead   = "workspace_read"
	toolWrite  = "workspace_write"
	toolList   = "workspace_list"
	toolSearch = "workspace_search"
	toolExec   = "workspace_exec"
)

func object(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type": "object", "properties": props,
		"required": required, "additionalProperties": false,
	}
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// remoteTools describes the executor-backed half. Every description says the
// work happens remotely, so a model reading only the tool list does not plan
// around a local filesystem it does not have.
func remoteTools() []any {
	pathArg := str("Path relative to the remote workspace root.")
	return []any{
		map[string]any{
			"name":        toolRead,
			"description": "Read a file from the REMOTE workspace. Paths are relative to the remote root; there is no local filesystem behind this tool.",
			"inputSchema": object(map[string]any{"path": pathArg}, "path"),
		},
		map[string]any{
			"name":        toolWrite,
			"description": "Replace a file in the REMOTE workspace, creating parent directories as needed.",
			"inputSchema": object(map[string]any{
				"path":    pathArg,
				"content": map[string]any{"type": "string", "maxLength": executor.MaxContent},
			}, "path", "content"),
		},
		map[string]any{
			"name":        toolList,
			"description": "List a directory in the REMOTE workspace. An entry marked alien is a symlink leaving the root and cannot be read or written.",
			"inputSchema": object(map[string]any{
				"path": str("Directory relative to the remote root; omit for the root itself."),
			}),
		},
		map[string]any{
			"name":        toolSearch,
			"description": "Search the REMOTE workspace with a Go regexp, one result per matching line. Results are capped; .git and node_modules are skipped.",
			"inputSchema": object(map[string]any{
				"pattern": str("Go regexp matched against each line."),
				"path":    str("Subtree to search, relative to the remote root; omit for all of it."),
			}, "pattern"),
		},
		map[string]any{
			"name":        toolExec,
			"description": "Run a command on the REMOTE executor and return its captured output. Output is truncated past a limit and the process is killed at the timeout.",
			"inputSchema": object(map[string]any{
				"argv": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
					"minItems": 1, "description": "Command and arguments, argv[0] included. Not a shell line.",
				},
				"cwd":        str("Working directory relative to the remote root; omit for the root."),
				"timeout_ms": map[string]any{"type": "integer", "minimum": 1},
			}, "argv"),
		},
	}
}

// Tools implements trustedstate.ToolSet.
func (g *Gateway) Tools() []any {
	return append(remoteTools(), g.state.Tools()...)
}

// ToolNames lists every tool the gateway serves, in the mcp__rca__ form the
// harness uses for --allowedTools. It is derived from the same descriptors the
// gateway advertises, so a tool cannot be served without being allowed.
func ToolNames() []string {
	names := []string{}
	for _, tool := range remoteTools() {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	for _, collection := range []string{"memory", "skills"} {
		for _, op := range []string{"list", "read", "put", "delete"} {
			names = append(names, collection+"_"+op)
		}
	}
	for i, name := range names {
		names[i] = "mcp__rca__" + name
	}
	return names
}

// Call implements trustedstate.ToolSet. Unknown names are refused here rather
// than forwarded: the executor's op set is not the tool set, and a name that
// fell through would be the model choosing a backend.
func (g *Gateway) Call(name string, args json.RawMessage) (any, error) {
	switch name {
	case toolRead, toolWrite, toolList, toolSearch, toolExec:
		return g.callRemote(name, args)
	}
	if strings.HasPrefix(name, "memory_") || strings.HasPrefix(name, "skills_") {
		return g.state.Call(name, args)
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

// remoteArgs is the decoded form of every executor-backed tool's arguments.
// One strict struct for all of them keeps an argument that belongs to another
// tool from being silently ignored.
type remoteArgs struct {
	Path      string   `json:"path"`
	Content   *string  `json:"content"`
	Pattern   string   `json:"pattern"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	TimeoutMS int      `json:"timeout_ms"`
}

func (g *Gateway) callRemote(name string, args json.RawMessage) (any, error) {
	if g.remote == nil {
		// Fail closed. A missing transport must not read as "no files here".
		return nil, errors.New("no remote executor is bound to this session")
	}
	var a remoteArgs
	if err := decodeStrict(args, &a, allowedArgs(name)); err != nil {
		return nil, err
	}
	req := &executor.Request{}
	switch name {
	case toolRead:
		req.Op, req.Path = executor.OpRead, a.Path
	case toolWrite:
		if a.Content == nil {
			return nil, errors.New("missing content")
		}
		req.Op, req.Path, req.Content = executor.OpWrite, a.Path, *a.Content
	case toolList:
		req.Op, req.Path = executor.OpList, a.Path
	case toolSearch:
		req.Op, req.Path, req.Pattern = executor.OpSearch, a.Path, a.Pattern
	case toolExec:
		req.Op, req.Argv, req.Cwd, req.TimeoutMS = executor.OpExec, a.Argv, a.Cwd, a.TimeoutMS
	}
	var result json.RawMessage
	if err := g.remote.Call(req, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// allowedArgs is the per-tool argument whitelist. Required names are checked
// separately by the executor, which owns the semantics.
func allowedArgs(name string) []string {
	switch name {
	case toolRead, toolList:
		return []string{"path"}
	case toolWrite:
		return []string{"path", "content"}
	case toolSearch:
		return []string{"path", "pattern"}
	case toolExec:
		return []string{"argv", "cwd", "timeout_ms"}
	}
	return nil
}

// decodeStrict rejects any argument the named tool does not take. A tolerated
// stray argument is how a caller ends up believing it passed a limit that was
// never read.
func decodeStrict(args json.RawMessage, target any, allowed []string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return errors.New("arguments must be an object")
	}
	permitted := map[string]bool{}
	for _, name := range allowed {
		permitted[name] = true
	}
	for name := range fields {
		if !permitted[name] {
			return fmt.Errorf("unknown argument %s", name)
		}
	}
	return json.Unmarshal(args, target)
}

// Ensure the gateway satisfies the interface the MCP plumbing serves.
var _ trustedstate.ToolSet = (*Gateway)(nil)
