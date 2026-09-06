package trustedstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	s, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}
func mutation(t *testing.T, s *Store, c, op string, q Request) Result {
	t.Helper()
	r, e := s.Mutate(c, op, q)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestLifecycleCASReplayAndCollections(t *testing.T) {
	s, root := openTest(t)
	q := Request{ID: "note", Content: "first", RequestID: "create"}
	r := mutation(t, s, "memory", "put", q)
	if r.Object.Revision != 1 || r.Error != "" {
		t.Fatal(r)
	}
	if r2 := mutation(t, s, "memory", "put", q); r2 != r {
		t.Fatal(r2)
	}
	r = mutation(t, s, "skills", "put", Request{ID: "note", Content: "skill", RequestID: "skill"})
	if r.Object.Revision != 1 {
		t.Fatal(r)
	}
	conflict := mutation(t, s, "memory", "put", Request{ID: "note", Content: "wrong", RequestID: "conflict"})
	if conflict.Error != "revision_conflict" {
		t.Fatal(conflict)
	}
	r = mutation(t, s, "memory", "put", Request{ID: "note", Content: "updated", ExpectedRevision: 1, RequestID: "update"})
	if r.Object.Revision != 2 {
		t.Fatal(r)
	}
	r = mutation(t, s, "memory", "delete", Request{ID: "note", ExpectedRevision: 2, RequestID: "delete"})
	if r.Object.Revision != 3 || !r.Object.Deleted {
		t.Fatal(r)
	}
	s.Close()
	s2, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s2.Close()
	if r2 := mutation(t, s2, "memory", "put", q); r2.Object.Content != "first" || r2.Object.Revision != 1 {
		t.Fatal(r2)
	}
	o, e := s2.Read("memory", "note")
	if e != nil || !o.Deleted || o.Revision != 3 {
		t.Fatal(o, e)
	}
	list, e := s2.List("memory")
	if e != nil || len(list) != 1 || !list[0].Deleted {
		t.Fatal(list, e)
	}
	r = mutation(t, s2, "memory", "put", Request{ID: "note", Content: "reborn", ExpectedRevision: 3, RequestID: "recreate"})
	if r.Object.Revision != 4 {
		t.Fatal(r)
	}
	if _, e = s2.Mutate("skills", "put", q); e == nil {
		t.Fatal("cross-collection request reuse accepted")
	}
	b, e := os.ReadFile(filepath.Join(root, "journal.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if len(lines) != 6 {
		t.Fatalf("got %d audit entries", len(lines))
	}
	var entry record
	if e = json.Unmarshal(lines[2], &entry); e != nil || entry.Result.Error != "revision_conflict" {
		t.Fatal(entry, e)
	}
}
func TestInvalidInputsAndLocks(t *testing.T) {
	s, root := openTest(t)
	if other, e := Open(root); e == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	for _, id := range []string{"../journal.jsonl", "/tmp/x", "a/b", "a\\b", ".", "..", "", strings.Repeat("a", 129), "%2e%2e"} {
		if _, e := s.Mutate("memory", "put", Request{ID: id, RequestID: "request"}); e == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	if _, e := s.Mutate("audit", "put", Request{ID: "x", RequestID: "request"}); e == nil {
		t.Fatal("audit exposed")
	}
	if _, e := s.Mutate("memory", "put", Request{ID: "x", Content: strings.Repeat("a", MaxContent+1), RequestID: "big"}); e == nil {
		t.Fatal("oversize accepted")
	}
	b, _ := os.ReadFile(filepath.Join(root, "journal.jsonl"))
	if len(b) != 0 {
		t.Fatal("invalid input mutated state")
	}
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("untouched"), 0600)
	symlinkRoot := filepath.Join(t.TempDir(), "root")
	os.Mkdir(symlinkRoot, 0700)
	os.Symlink(target, filepath.Join(symlinkRoot, "journal.jsonl"))
	if other, e := Open(symlinkRoot); e == nil {
		other.Close()
		t.Fatal("followed journal symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "untouched" {
		t.Fatal("target changed")
	}
}

type brokenJournal struct {
	journal
	short    bool
	syncFail bool
}

func (f brokenJournal) Write(b []byte) (int, error) {
	if f.short {
		return len(b) / 2, nil
	}
	return f.journal.Write(b)
}
func (f brokenJournal) Sync() error {
	if f.syncFail {
		return errors.New("injected fsync failure")
	}
	return f.journal.Sync()
}
func TestPersistenceFailureStopsService(t *testing.T) {
	for _, short := range []bool{true, false} {
		t.Run(map[bool]string{true: "short_write", false: "fsync"}[short], func(t *testing.T) {
			s, root := openTest(t)
			s.file = brokenJournal{journal: s.file, short: short, syncFail: !short}
			q := Request{ID: "x", RequestID: "q", Content: "value"}
			if _, e := s.Mutate("memory", "put", q); e == nil {
				t.Fatal("reported success")
			}
			if _, e := s.Read("memory", "x"); e == nil {
				t.Fatal("continued reads after failure")
			}
			if _, e := s.Mutate("memory", "put", q); e == nil {
				t.Fatal("continued writes after failure")
			}
			s.Close()
			if !short {
				s2, e := Open(root)
				if e != nil {
					t.Fatal(e)
				}
				defer s2.Close()
				r := mutation(t, s2, "memory", "put", q)
				if r.Object.Revision != 1 || s2.sequence != 1 {
					t.Fatal("retry duplicated uncertain mutation")
				}
			}
		})
	}
}
func TestRejectIncompleteAndInconsistentJournal(t *testing.T) {
	for _, suffix := range []string{"{\"partial\":", "{}\n"} {
		t.Run(suffix, func(t *testing.T) {
			s, root := openTest(t)
			mutation(t, s, "memory", "put", Request{ID: "x", RequestID: "q"})
			s.Close()
			f, e := os.OpenFile(filepath.Join(root, "journal.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				t.Fatal(e)
			}
			f.WriteString(suffix)
			f.Close()
			if other, e := Open(root); e == nil {
				other.Close()
				t.Fatal("corrupt journal accepted")
			}
		})
	}
}
func TestEscapedContentReplay(t *testing.T) {
	s, root := openTest(t)
	content := strings.Repeat("\x00", MaxContent)
	mutation(t, s, "memory", "put", Request{ID: "x", RequestID: "q", Content: content})
	s.Close()
	s2, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s2.Close()
	o, e := s2.Read("memory", "x")
	if e != nil || o.Content != content {
		t.Fatal("content changed", e)
	}
}
func TestMCP(t *testing.T) {
	s, _ := openTest(t)
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_put","arguments":{"id":"x","content":"one","expected_revision":0,"request_id":"q"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory_read","arguments":{"id":"x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"shell","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"skills_put","arguments":{"id":"x","content":"one","request_id":"q2"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"skills_put","arguments":{"id":"x","content":"one","expected_revision":0,"request_id":"q3","root":"/tmp"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"memory_delete","arguments":{"id":"x","expected_revision":null,"request_id":"q4"}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"memory_delete","arguments":{"id":"x","expected_revision":1,"request_id":"q5"}}}`,
	}
	var out bytes.Buffer
	if e := Serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, s); e != nil {
		t.Fatal(e)
	}
	var replies []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var r map[string]any
		if e := json.Unmarshal(l, &r); e != nil {
			t.Fatal(e)
		}
		replies = append(replies, r)
	}
	if len(replies) != 8 {
		t.Fatal(out.String())
	}
	if len(replies[1]["result"].(map[string]any)["tools"].([]any)) != 8 {
		t.Fatal("wrong tool set")
	}
	for i := 4; i < 8; i++ {
		if replies[i]["result"].(map[string]any)["isError"] != true {
			t.Fatal(replies[i])
		}
	}
	o, e := s.Read("memory", "x")
	if e != nil || o.Revision != 1 || o.Deleted || s.sequence != 1 {
		t.Fatal(o, e, s.sequence)
	}
}
