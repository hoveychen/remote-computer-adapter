package toolgateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hoveychen/remote-computer-adapter/internal/executor"
	"github.com/hoveychen/remote-computer-adapter/internal/trustedstate"
)

// fakeRemote records what the gateway forwarded, so the tests assert on the
// request that would have crossed the transport rather than on a mock's
// convenience API.
type fakeRemote struct {
	seen []executor.Request
	err  error
	body string
}

func (f *fakeRemote) Call(req *executor.Request, result any) error {
	f.seen = append(f.seen, *req)
	if f.err != nil {
		return f.err
	}
	body := f.body
	if body == "" {
		body = `{"ok":true}`
	}
	return json.Unmarshal([]byte(body), result)
}

// openStore makes the private store directory trustedstate insists on: the
// state root is the trusted side's alone, so a group- or world-readable one is
// refused rather than tightened behind the operator's back.
func openStore(t *testing.T) *trustedstate.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := trustedstate.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newGateway(t *testing.T) (*Gateway, *fakeRemote) {
	t.Helper()
	remote := &fakeRemote{}
	return New(openStore(t), remote), remote
}

func call(t *testing.T, g *Gateway, name, args string) (any, error) {
	t.Helper()
	return g.Call(name, json.RawMessage(args))
}

func TestAdvertisedToolsMatchAllowedNames(t *testing.T) {
	g, _ := newGateway(t)
	advertised := []string{}
	for _, tool := range g.Tools() {
		advertised = append(advertised, "mcp__rca__"+tool.(map[string]any)["name"].(string))
	}
	allowed := ToolNames()
	slices.Sort(advertised)
	slices.Sort(allowed)
	if !slices.Equal(advertised, allowed) {
		t.Fatalf("advertised %v\nallowed    %v", advertised, allowed)
	}
	for _, name := range allowed {
		if !strings.HasPrefix(name, "mcp__rca__") {
			t.Errorf("%q is not namespaced", name)
		}
	}
}

// Every advertised tool must be callable. A tool in the list that Call refuses
// is a tool the model will try and fail on, repeatedly.
func TestEveryAdvertisedToolDispatches(t *testing.T) {
	g, _ := newGateway(t)
	for _, tool := range g.Tools() {
		name := tool.(map[string]any)["name"].(string)
		if _, err := call(t, g, name, `{}`); err != nil && strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("advertised tool %q is not dispatched", name)
		}
	}
}

func TestUnknownToolRefused(t *testing.T) {
	g, remote := newGateway(t)
	for _, name := range []string{"Bash", "workspace_delete", "fs.read", "memory", ""} {
		if _, err := call(t, g, name, `{}`); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
	if len(remote.seen) != 0 {
		t.Fatalf("an unknown tool reached the executor: %+v", remote.seen)
	}
}

func TestRemoteToolsForwardTheirArguments(t *testing.T) {
	g, remote := newGateway(t)
	if _, err := call(t, g, toolRead, `{"path":"src/main.go"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, g, toolExec, `{"argv":["go","test"],"cwd":"sub","timeout_ms":5000}`); err != nil {
		t.Fatal(err)
	}
	if len(remote.seen) != 2 {
		t.Fatalf("forwarded %d requests, want 2", len(remote.seen))
	}
	if remote.seen[0].Op != executor.OpRead || remote.seen[0].Path != "src/main.go" {
		t.Errorf("read forwarded as %+v", remote.seen[0])
	}
	exec := remote.seen[1]
	if exec.Op != executor.OpExec || !slices.Equal(exec.Argv, []string{"go", "test"}) ||
		exec.Cwd != "sub" || exec.TimeoutMS != 5000 {
		t.Errorf("exec forwarded as %+v", exec)
	}
}

// An argument the tool does not take must be refused, not dropped: a silently
// ignored timeout_ms is a caller believing it set a bound it never set.
func TestStrayArgumentsRefused(t *testing.T) {
	g, remote := newGateway(t)
	for name, args := range map[string]string{
		toolRead:  `{"path":"a","timeout_ms":10}`,
		toolWrite: `{"path":"a","content":"b","argv":["x"]}`,
		toolExec:  `{"argv":["ls"],"pattern":"x"}`,
		toolList:  `{"path":"a","local":true}`,
	} {
		if _, err := call(t, g, name, args); err == nil {
			t.Errorf("%s accepted a stray argument", name)
		}
	}
	if len(remote.seen) != 0 {
		t.Fatalf("a rejected call still reached the executor: %+v", remote.seen)
	}
}

// The write tool must distinguish an absent body from an empty one: treating
// absent as "" would silently truncate a file the model meant to leave alone.
func TestWriteRequiresAnExplicitBody(t *testing.T) {
	g, remote := newGateway(t)
	if _, err := call(t, g, toolWrite, `{"path":"a"}`); err == nil {
		t.Fatal("a write with no content was accepted")
	}
	if len(remote.seen) != 0 {
		t.Fatal("the contentless write reached the executor")
	}
	if _, err := call(t, g, toolWrite, `{"path":"a","content":""}`); err != nil {
		t.Fatalf("an explicitly empty write was rejected: %v", err)
	}
}

// With no transport bound, remote tools must fail rather than return nothing:
// an empty result reads to the model as an empty workspace.
func TestRemoteToolsFailClosedWithoutATransport(t *testing.T) {
	g := New(openStore(t), nil)
	for _, name := range []string{toolRead, toolWrite, toolList, toolSearch, toolExec} {
		result, err := call(t, g, name, `{}`)
		if err == nil {
			t.Errorf("%s returned %v instead of failing", name, result)
		}
	}
}

func TestTransportErrorSurfaces(t *testing.T) {
	g, remote := newGateway(t)
	remote.err = errors.New("executor transport closed")
	if _, err := call(t, g, toolRead, `{"path":"a"}`); err == nil ||
		!strings.Contains(err.Error(), "transport closed") {
		t.Fatalf("err = %v, want the transport failure", err)
	}
}

// The two halves must stay separate: a state tool takes a logical ID and can
// never name a path, and a workspace tool can never reach the trusted store.
func TestStateToolsAreNotPathAddressed(t *testing.T) {
	g, remote := newGateway(t)
	if _, err := call(t, g, "memory_put",
		`{"id":"note","content":"x","expected_revision":0,"request_id":"r1"}`); err != nil {
		t.Fatal(err)
	}
	if len(remote.seen) != 0 {
		t.Fatal("a state tool reached the executor")
	}
	// A path-shaped ID must be refused by the store's own ID rules.
	if _, err := call(t, g, "memory_put",
		`{"id":"../../etc/passwd","content":"x","expected_revision":0,"request_id":"r2"}`); err == nil {
		t.Error("a path-shaped memory ID was accepted")
	}
	// And a workspace tool has no way to address the store.
	if _, err := call(t, g, toolRead, `{"path":"note","collection":"memory"}`); err == nil {
		t.Error("a workspace tool accepted a state argument")
	}
}

func TestStateMutationsStillRequireCAS(t *testing.T) {
	g, _ := newGateway(t)
	if _, err := call(t, g, "memory_put", `{"id":"n","content":"x"}`); err == nil {
		t.Fatal("a mutation without expected_revision or request_id was accepted")
	}
}
