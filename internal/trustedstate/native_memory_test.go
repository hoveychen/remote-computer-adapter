package trustedstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const testNote = "2026-09-06T12-13-14-decisions.md"

func ptr[T any](v T) *T { return &v }
func TestNativeMemoryLifecycleAndFormat(t *testing.T) {
	s, _ := openTest(t)
	actor := NativeActor{Kind: "model_tool", ThreadID: "thread"}
	r, e := s.MemoryNote(actor, "call", testNote, "one\ntwo\nthree\n")
	if e != nil || r.Error != "" {
		t.Fatal(r, e)
	}
	r2, e := s.MemoryNote(actor, "call", testNote, "one\ntwo\nthree\n")
	if e != nil || !reflect.DeepEqual(r, r2) {
		t.Fatal(r2, e)
	}
	if _, e = s.MemoryNote(actor, "call", testNote, "different"); e == nil {
		t.Fatal("changed call accepted")
	}
	r, e = s.MemoryNote(actor, "other", testNote, "other")
	if e != nil || r.Error != "revision_conflict" {
		t.Fatal(r, e)
	}
	root, e := s.MemoryList(MemoryListRequest{MaxResults: 10})
	if e != nil || !reflect.DeepEqual(root.Entries, []MemoryEntry{{Path: "extensions", EntryType: "directory"}}) {
		t.Fatal(root, e)
	}
	list, e := s.MemoryList(MemoryListRequest{Path: ptr("extensions/ad_hoc/notes"), MaxResults: 10})
	if e != nil || len(list.Entries) != 1 || list.Entries[0].Path != notePrefix+testNote {
		t.Fatal(list, e)
	}
	read, e := s.MemoryRead(MemoryReadRequest{Path: notePrefix + testNote, LineOffset: 2, MaxLines: ptr(1)})
	if e != nil || read.Content != "two\n" || !read.Truncated || read.StartLineNumber != 2 {
		t.Fatal(read, e)
	}
	read, e = s.MemoryRead(MemoryReadRequest{Path: notePrefix + testNote, LineOffset: 4})
	if e != nil || read.Content != "" || read.Truncated {
		t.Fatal(read, e)
	}
	if _, e = s.MemoryRead(MemoryReadRequest{Path: notePrefix + testNote, LineOffset: 5}); e == nil {
		t.Fatal("invalid offset")
	}
	for _, filename := range []string{"../x.md", "note.md", "2026-09-06T12-13-14-BAD.md", "2026-09-06T12-13-14-.md", "2026-09-06T12-13-14-" + strings.Repeat("a", 81) + ".md"} {
		if _, e = s.MemoryNote(actor, "bad", filename, "note"); e == nil {
			t.Fatal(filename)
		}
	}
	if _, e = s.MemoryNote(actor, "empty", testNote, " \n"); e == nil {
		t.Fatal("empty note")
	}
	// Native structs match the Codex serialized backend response fields exactly.
	b, _ := json.Marshal(read)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	if len(fields) != 4 || fields["start_line_number"] != float64(4) {
		t.Fatal(string(b))
	}
}
func TestNativeMemoryCursorAndByteBound(t *testing.T) {
	s, _ := openTest(t)
	batch(t, s, nativeQ("seed", NativeChange{Domain: "memory.artifact", Key: "a.md", Content: []byte("alpha")}, NativeChange{Domain: "memory.artifact", Key: "b.md", Content: []byte(strings.Repeat("你", 20000))}))
	q := MemoryListRequest{MaxResults: 1}
	first, e := s.MemoryList(q)
	if e != nil || first.NextCursor == nil || !first.Truncated {
		t.Fatal(first, e)
	}
	q.Cursor = first.NextCursor
	second, e := s.MemoryList(q)
	if e != nil || second.Entries[0].Path != "b.md" {
		t.Fatal(second, e)
	}
	altered := q
	altered.Path = ptr("a.md")
	if _, e = s.MemoryList(altered); e == nil {
		t.Fatal("cursor reused with another query")
	}
	batch(t, s, nativeQ("change", NativeChange{Domain: "memory.artifact", Key: "a.md", ExpectedRevision: 1, Content: []byte("new")}))
	if _, e = s.MemoryList(q); e == nil || e.Error() != "snapshot_expired" {
		t.Fatal(e)
	}
	read, e := s.MemoryRead(MemoryReadRequest{Path: "b.md", LineOffset: 1})
	if e != nil || !read.Truncated || len(read.Content) > NativePageBytes || !utf8.ValidString(read.Content) {
		t.Fatal(e, len(read.Content))
	}
	for _, p := range []string{"/etc/passwd", "../secret", "a/../b.md", "a\\b"} {
		if _, e = s.MemoryList(MemoryListRequest{Path: &p, MaxResults: 1}); e == nil {
			t.Fatal(p)
		}
	}
	r := batch(t, s, nativeQ("collision", NativeChange{Domain: "memory.extension", Key: "ad_hoc/notes/x.md", Content: []byte("X")}, note("x.md", "Y", 0)))
	if r.Error != "memory_path_conflict" {
		t.Fatal(r)
	}
	r = batch(t, s, nativeQ("parent-collision", NativeChange{Domain: "memory.artifact", Key: "a.md/sub", Content: []byte("X")}))
	if r.Error != "memory_path_conflict" {
		t.Fatal(r)
	}
}
func TestNativeMemorySearchModes(t *testing.T) {
	s, _ := openTest(t)
	batch(t, s, nativeQ("seed", NativeChange{Domain: "memory.artifact", Key: "a.md", Content: []byte("Alpha\nbeta\nalpha beta\nno match\nA_L-PHA\n")}))
	base := MemorySearchRequest{Queries: []string{" alpha ", "beta"}, MatchMode: MemoryMatchMode{Type: "any"}, MaxResults: 10}
	r, e := s.MemorySearch(base)
	if e != nil || len(r.Matches) != 3 || r.Queries[0] != "alpha" {
		t.Fatal(r, e)
	}
	q := base
	q.MatchMode.Type = "all_on_same_line"
	r, e = s.MemorySearch(q)
	if e != nil || len(r.Matches) != 1 || r.Matches[0].MatchLineNumber != 3 {
		t.Fatal(r, e)
	}
	q = base
	q.MatchMode = MemoryMatchMode{Type: "all_within_lines", LineCount: 2}
	r, e = s.MemorySearch(q)
	if e != nil || len(r.Matches) != 2 || r.Matches[0].Content != "Alpha\nbeta" || r.Matches[1].MatchLineNumber != 3 {
		t.Fatal(r, e)
	}
	q = base
	q.Queries = []string{"alpha"}
	q.Normalized = true
	r, e = s.MemorySearch(q)
	if e != nil || len(r.Matches) != 3 {
		t.Fatal(r, e)
	}
	q.CaseSensitive = true
	r, e = s.MemorySearch(q)
	if e != nil || len(r.Matches) != 1 {
		t.Fatal(r, e)
	}
	q = base
	q.MaxResults = 1
	r, e = s.MemorySearch(q)
	if e != nil || r.NextCursor == nil {
		t.Fatal(r, e)
	}
	q.Cursor = r.NextCursor
	r, e = s.MemorySearch(q)
	if e != nil || r.Matches[0].MatchLineNumber != 2 {
		t.Fatal(r, e)
	}
	q.ContextLines = 1
	if _, e = s.MemorySearch(q); e == nil {
		t.Fatal("search cursor not query bound")
	}
}
func TestLegacyMemoryFreezeAtomicReplay(t *testing.T) {
	s, root := openTest(t)
	mutation(t, s, "memory", "put", Request{ID: "old", RequestID: "old-create", Content: "legacy"})
	if _, e := s.ImportLegacyMemory("bad-import", "old", testNote, 2); e == nil {
		t.Fatal("bad source revision accepted")
	}
	mutation(t, s, "memory", "put", Request{ID: "old", RequestID: "before-import", Content: "updated", ExpectedRevision: 1})
	r, e := s.ImportLegacyMemory("import", "old", testNote, 2)
	if e != nil || r.Error != "" {
		t.Fatal(r, e)
	}
	r2, e := s.ImportLegacyMemory("import", "old", testNote, 2)
	if e != nil || !reflect.DeepEqual(r, r2) {
		t.Fatal(r2, e)
	}
	frozen := mutation(t, s, "memory", "put", Request{ID: "old", RequestID: "after-import", Content: "fork", ExpectedRevision: 2})
	if frozen.Error != "migrated_read_only" {
		t.Fatal(frozen)
	}
	s.Close()
	s, e = Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	read, e := s.MemoryRead(MemoryReadRequest{Path: notePrefix + testNote, LineOffset: 1})
	if e != nil || read.Content != "updated" {
		t.Fatal(read, e)
	}
	// A failed target CAS must leave the source writable.
	mutation(t, s, "memory", "put", Request{ID: "other", RequestID: "other-create", Content: "other"})
	r, e = s.ImportLegacyMemory("failed-import", "other", testNote, 1)
	if e != nil || r.Error != "revision_conflict" {
		t.Fatal(r, e)
	}
	unblocked := mutation(t, s, "memory", "put", Request{ID: "other", RequestID: "still-writable", ExpectedRevision: 1, Content: "okay"})
	if unblocked.Error != "" {
		t.Fatal(unblocked)
	}
}
func TestNativeHTTPAuthScopesAndSummary(t *testing.T) {
	s, root := openTest(t)
	modelToken := strings.Repeat("m", 32)
	backgroundToken := strings.Repeat("b", 32)
	creds := []NativeCredential{{Token: modelToken, Kind: "model_tool", ThreadID: "thread"}, {Token: backgroundToken, Kind: "background", ThreadID: "thread"}}
	h, e := NativeHTTPHandler(s, creds)
	if e != nil {
		t.Fatal(e)
	}
	call := func(handler http.Handler, token, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		q := httptest.NewRequest("POST", path, strings.NewReader(body))
		q.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, q)
		return w
	}
	handshake := call(h, modelToken, "/native/v2/handshake", "{}")
	if handshake.Code != 200 {
		t.Fatal(handshake.Body.String())
	}
	var identity map[string]any
	_ = json.Unmarshal(handshake.Body.Bytes(), &identity)
	backgroundHandshake := call(h, backgroundToken, "/native/v2/handshake", "{}")
	if backgroundHandshake.Code != 200 || !strings.Contains(backgroundHandshake.Body.String(), "native_memory_jobs") || !strings.Contains(backgroundHandshake.Body.String(), "native_memory_consolidation") {
		t.Fatal(backgroundHandshake.Code, backgroundHandshake.Body.String())
	}
	enqueueBody := `{"request_id":"enqueue-http","job_id":"rollout-http","input_version":"v1"}`
	if w := call(h, modelToken, "/native/v2/memory.stage1.enqueue", enqueueBody); w.Code != 403 {
		t.Fatal("model token reached background endpoint", w.Code)
	}
	if w := call(h, backgroundToken, "/native/v2/memory.stage1.enqueue", enqueueBody); w.Code != 200 || w.Header().Get("X-RCA-Commit-Sequence") == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	claimHTTP := call(h, backgroundToken, "/native/v2/memory.job.claim", `{"request_id":"claim-http","job_id":"rollout-http","lease_seconds":60}`)
	if claimHTTP.Code != 200 || !strings.Contains(claimHTTP.Body.String(), "lease_token") || strings.Contains(claimHTTP.Body.String(), strings.Repeat("m", 32)) {
		t.Fatal(claimHTTP.Code, claimHTTP.Body.String())
	}
	for _, token := range []string{"", strings.Repeat("x", 32)} {
		if w := call(h, token, "/native/v2/handshake", "{}"); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	noteBody := fmt.Sprintf(`{"call_id":"call","filename":%q,"note":"hello"}`, testNote)
	w := call(h, modelToken, "/native/v2/memory.note.create", noteBody)
	if w.Code != 200 || w.Body.String() != "{}\n" || w.Header().Get("X-RCA-Commit-Sequence") == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call(h, backgroundToken, "/native/v2/memory.note.create", noteBody); w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`null`, `{"actor":"maintenance"}`, `{"domain":"maintenance"}`, `{"root":"/tmp"}`, `{} {}`} {
		if w = call(h, modelToken, "/native/v2/handshake", body); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	if w = call(h, modelToken, "/native/v2/resources.batch", "{}"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	legacy := HTTPHandler(s, modelToken)
	if w = call(legacy, modelToken, "/native/v2/handshake", "{}"); w.Code != 404 {
		t.Fatal("MCP token enabled admin API")
	}
	if w = call(h, modelToken, "/native/v2/memory.summary.read", "{}"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	batch(t, s, nativeQ("summary", NativeChange{Domain: "memory.artifact", Key: "memory_summary.md", Content: []byte("canonical summary")}))
	if w = call(h, modelToken, "/native/v2/memory.resource.read", `{"domain":"memory.artifact","key":"memory_summary.md","revision":1}`); w.Code != 403 {
		t.Fatal("model token reached raw resource endpoint", w.Code)
	}
	resource := call(h, backgroundToken, "/native/v2/memory.resource.read", `{"domain":"memory.artifact","key":"memory_summary.md","revision":1}`)
	if resource.Code != 200 || !strings.Contains(resource.Body.String(), "Y2Fub25pY2FsIHN1bW1hcnk=") {
		t.Fatal(resource.Code, resource.Body.String())
	}
	projection := call(h, backgroundToken, "/native/v2/memory.projection", `{"include_resources":false}`)
	if projection.Code != 200 || strings.Contains(projection.Body.String(), "canonical summary") || strings.Contains(projection.Body.String(), "content_base64") {
		t.Fatal(projection.Code, projection.Body.String())
	}
	summary := call(h, modelToken, "/native/v2/memory.summary.read", "{}")
	read := call(h, modelToken, "/native/v2/memory.read", `{"path":"memory_summary.md","line_offset":1}`)
	if summary.Code != 200 || !bytes.Equal(summary.Body.Bytes(), read.Body.Bytes()) {
		t.Fatal(summary.Body.String(), read.Body.String())
	}
	s.Close()
	if w = call(h, modelToken, "/native/v2/handshake", "{}"); w.Code != 503 {
		t.Fatal(w.Code)
	}
	s, e = Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	h, e = NativeHTTPHandler(s, creds)
	if e != nil {
		t.Fatal(e)
	}
	again := call(h, modelToken, "/native/v2/handshake", "{}")
	if !bytes.Equal(handshake.Body.Bytes(), again.Body.Bytes()) {
		t.Fatal("store identity changed")
	}
	if _, e = NativeHTTPHandler(s, []NativeCredential{creds[0], creds[0]}); e == nil {
		t.Fatal("duplicate tokens accepted")
	}
}

func TestNativeReadExplicitSnapshot(t *testing.T) {
	s, _ := openTest(t)
	batch(t, s, nativeQ("seed", NativeChange{Domain: "memory.artifact", Key: "a.md", Content: []byte("old")}))
	list, e := s.MemoryList(MemoryListRequest{MaxResults: 1})
	if e != nil {
		t.Fatal(e)
	}
	q := MemoryReadRequest{Path: "a.md", LineOffset: 1, ExpectedSequence: &list.snapshot}
	first, e := s.MemoryRead(q)
	if e != nil || first.Content != "old" {
		t.Fatal(first, e)
	}
	batch(t, s, nativeQ("update", NativeChange{Domain: "memory.artifact", Key: "a.md", ExpectedRevision: 1, Content: []byte("new")}))
	if _, e = s.MemoryRead(q); e == nil || e.Error() != "snapshot_expired" {
		t.Fatal("snapshot silently read new content", e)
	}
}
