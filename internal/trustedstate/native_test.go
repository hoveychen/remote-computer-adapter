package trustedstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func nativeQ(id string, changes ...NativeChange) NativeRequest {
	return NativeRequest{RequestID: id, Operation: "resources.batch", Actor: NativeActor{Kind: "maintenance"}, Changes: changes}
}
func note(key, content string, rev uint64) NativeChange {
	return NativeChange{Domain: "memory.note", Key: key, Content: []byte(content), ExpectedRevision: rev}
}
func batch(t *testing.T, s *Store, q NativeRequest) NativeResult {
	t.Helper()
	r, e := s.NativeBatch(q)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func skill(rev uint64) NativeChange {
	return NativeChange{Domain: "skills.package", Key: "demo", ExpectedRevision: rev, Package: &NativePackage{Authority: "user", Enabled: true, Files: []NativeFile{{Path: "SKILL.md", Content: []byte("---\nname: demo\ndescription: Demo\n---\nUse assets/data.txt")}, {Path: "assets/data.txt", Content: []byte("original")}}}}
}
func TestNativeAtomicCASMixedReplay(t *testing.T) {
	s, root := openTest(t)
	mutation(t, s, "memory", "put", Request{ID: "legacy", RequestID: "v1", Content: "old"})
	q := nativeQ("v2", note("a.md", "A", 0), note("b.md", "B", 0))
	first := batch(t, s, q)
	if first.Status != "committed" || first.CommitSequence != 2 || len(first.Resources) != 2 {
		t.Fatal(first)
	}
	conflict := nativeQ("conflict", note("a.md", "new A", 1), note("b.md", "new B", 0))
	if r := batch(t, s, conflict); r.Error != "revision_conflict" || len(r.Resources) != 0 {
		t.Fatal(r)
	}
	for _, key := range []string{"a.md", "b.md"} {
		o, e := s.NativeRead("memory.note", key, 1)
		if e != nil || o.Revision != 1 {
			t.Fatal(o, e)
		}
	}
	mutation(t, s, "skills", "put", Request{ID: "legacy", RequestID: "later-v1", Content: "old skill"})
	if _, e := s.Mutate("memory", "put", Request{ID: "x", RequestID: "v2"}); e == nil {
		t.Fatal("v1 reused v2 id")
	}
	if _, e := s.NativeBatch(nativeQ("v1", note("c.md", "C", 0))); e == nil {
		t.Fatal("v2 reused v1 id")
	}
	q.Changes[0].Content = []byte("changed")
	if _, e := s.NativeBatch(q); e == nil {
		t.Fatal("accepted changed retry")
	}
	q.Changes[0].Content = []byte("A")
	s.Close()
	s, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if r := batch(t, s, q); !reflect.DeepEqual(r, first) {
		t.Fatal(r, first)
	}
	if r := batch(t, s, conflict); r.Error != "revision_conflict" || r.CommitSequence != 3 {
		t.Fatal(r)
	}
	if s.sequence != 4 {
		t.Fatal(s.sequence)
	}
	// v1 decoder cannot interpret a v2 transaction as a valid legacy mutation.
	b, _ := os.ReadFile(filepath.Join(root, "journal.jsonl"))
	var legacy record
	_ = json.Unmarshal(bytes.Split(b, []byte("\n"))[1], &legacy)
	if validate(legacy.Collection, legacy.Operation, legacy.Request) == nil {
		t.Fatal("old implementation would accept v2")
	}
}
func TestNativePackageTombstoneSnapshotAndOwnership(t *testing.T) {
	s, root := openTest(t)
	q := nativeQ("install", skill(0))
	r := batch(t, s, q)
	if r.Error != "" {
		t.Fatal(r)
	}
	q.Changes[0].Package.Files[0].Content[0] = 'X'
	r.Resources[0].Package.Files[0].Content[0] = 'X'
	got, e := s.NativeRead("skills.package", "demo", 1)
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range got.Package.Files {
		if f.SHA256 != hashBytes(f.Content) {
			t.Fatal("caller modified canonical content")
		}
	}
	updated := skill(1)
	updated.Package.Files = updated.Package.Files[:1]
	batch(t, s, nativeQ("upgrade", updated))
	if _, e = s.NativeRead("skills.package", "demo", 1); e == nil {
		t.Fatal("old snapshot silently read new package")
	}
	got, e = s.NativeRead("skills.package", "demo", 2)
	if e != nil || len(got.Package.Files) != 1 {
		t.Fatal(got, e)
	}
	del := NativeChange{Domain: "skills.package", Key: "demo", ExpectedRevision: 2, Deleted: true}
	if r = batch(t, s, nativeQ("delete", del)); !r.Resources[0].Deleted || r.Resources[0].Revision != 3 {
		t.Fatal(r)
	}
	if r = batch(t, s, nativeQ("reborn", skill(3))); r.Resources[0].Revision != 4 {
		t.Fatal(r)
	}
	s.Close()
	s, e = Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	got, e = s.NativeRead("skills.package", "demo", 4)
	if e != nil || len(got.Package.Files) != 2 {
		t.Fatal(got, e)
	}
}
func TestNativeRejectLimitsAndPaths(t *testing.T) {
	s, _ := openTest(t)
	cases := []NativeRequest{nativeQ("empty"), nativeQ("duplicate", note("a.md", "A", 0), note("a.md", "B", 0)), nativeQ("large", note("a.md", strings.Repeat("x", NativeResourceBytes+1), 0))}
	for _, p := range []string{"/tmp/x", "../x", "a/../x", "a//x", "a\\x", "a\x00x", "."} {
		cases = append(cases, nativeQ("badpath", note(p, "A", 0)))
	}
	for _, count := range []int{513, 17} {
		c := skill(0)
		for i := 1; i < count; i++ {
			size := 1
			if count == 17 {
				size = NativeResourceBytes
			}
			c.Package.Files = append(c.Package.Files, NativeFile{Path: fmt.Sprintf("file%d", i), Content: bytes.Repeat([]byte("x"), size)})
		}
		cases = append(cases, nativeQ("size", c))
	}
	c := skill(0)
	c.Package.Files = append(c.Package.Files, c.Package.Files[0])
	cases = append(cases, nativeQ("duplicate-file", c))
	c = skill(0)
	c.Package.Files = c.Package.Files[1:]
	cases = append(cases, nativeQ("missing-skill", c))
	many := make([]NativeChange, NativeTransactionChanges+1)
	cases = append(cases, nativeQ("many", many...))
	encoded := []NativeChange{}
	for i := 0; i < 25; i++ {
		encoded = append(encoded, note(fmt.Sprintf("%d.md", i), strings.Repeat("x", NativeResourceBytes), 0))
	}
	cases = append(cases, nativeQ("encoded-limit", encoded...))
	for i, q := range cases {
		if _, e := s.NativeBatch(q); e == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if s.sequence != 0 {
		t.Fatal("invalid batch changed journal")
	}
}

// This fault writes a real prefix; reopen must reject the incomplete tail.
type partialNativeJournal struct{ journal }

func (f partialNativeJournal) Write(b []byte) (int, error) { return f.journal.Write(b[:len(b)/2]) }
func TestNativePersistenceFaults(t *testing.T) {
	for _, mode := range []string{"short", "sync"} {
		t.Run(mode, func(t *testing.T) {
			s, root := openTest(t)
			if mode == "short" {
				s.file = partialNativeJournal{s.file}
			} else {
				s.file = brokenJournal{journal: s.file, syncFail: true}
			}
			q := nativeQ("uncertain", note("a.md", "A", 0), skill(0))
			if _, e := s.NativeBatch(q); e == nil {
				t.Fatal("false success")
			}
			if len(s.native) != 0 || s.sequence != 0 {
				t.Fatal("published unpersisted batch")
			}
			if _, e := s.NativeRead("memory.note", "a.md", 0); e == nil {
				t.Fatal("read after failure")
			}
			if _, e := s.NativeBatch(q); e == nil {
				t.Fatal("write after failure")
			}
			s.Close()
			s2, e := Open(root)
			if mode == "short" {
				if e == nil {
					s2.Close()
					t.Fatal("accepted truncated transaction")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer s2.Close()
			r := batch(t, s2, q)
			if r.CommitSequence != 1 || s2.sequence != 1 || len(r.Resources) != 2 {
				t.Fatal(r)
			}
		})
	}
}
func TestNativeConcurrentCAS(t *testing.T) {
	s, _ := openTest(t)
	var wg sync.WaitGroup
	results := make(chan NativeResult, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, e := s.NativeBatch(nativeQ(fmt.Sprintf("q%d", i), note("a.md", "A", 0), note("b.md", "B", 0)))
			results <- r
			errs <- e
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	committed := 0
	for r := range results {
		if r.Status == "committed" {
			committed++
		} else if r.Error != "revision_conflict" {
			t.Fatal(r)
		}
	}
	if committed != 1 || s.sequence != 20 {
		t.Fatal(committed, s.sequence)
	}
}
func TestNativeRejectJournalTampering(t *testing.T) {
	for _, field := range []string{"digest", "result", "version", "sequence", "duplicate", "missing-newline", "oversize"} {
		t.Run(field, func(t *testing.T) {
			s, root := openTest(t)
			batch(t, s, nativeQ("q", note("a.md", "A", 0)))
			s.Close()
			p := filepath.Join(root, "journal.jsonl")
			b, _ := os.ReadFile(p)
			var r nativeRecord
			_ = json.Unmarshal(b, &r)
			switch field {
			case "digest":
				r.RequestDigest = "wrong"
			case "result":
				r.Result.Status = "wrong"
			case "version":
				r.FormatVersion = 3
			case "sequence":
				r.Sequence = 4
			}
			if field == "duplicate" {
				b = append(b, b...)
			} else if field == "missing-newline" {
				b = bytes.TrimSuffix(b, []byte("\n"))
			} else if field == "oversize" {
				b = bytes.Repeat([]byte("x"), NativeTransactionBytes+1)
			} else {
				b, _ = json.Marshal(r)
				b = append(b, '\n')
			}
			if e := os.WriteFile(p, b, 0600); e != nil {
				t.Fatal(e)
			}
			if other, e := Open(root); e == nil {
				other.Close()
				t.Fatal("tampered journal accepted")
			}
		})
	}
}

func TestNativeFullSizedPackage(t *testing.T) {
	s, root := openTest(t)
	c := skill(0)
	c.Package.Files = nil
	skillHeader := []byte("---\nname: demo\ndescription: Full-sized package boundary test.\n---\n")
	for i := 0; i < 16; i++ {
		p := fmt.Sprintf("assets/%d", i)
		content := bytes.Repeat([]byte("x"), NativeResourceBytes)
		if i == 0 {
			p = "SKILL.md"
			content = append(skillHeader, bytes.Repeat([]byte("x"), NativeResourceBytes-len(skillHeader))...)
		}
		c.Package.Files = append(c.Package.Files, NativeFile{Path: p, Content: content})
	}
	r := batch(t, s, nativeQ("full-package", c))
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	s.Close()
	s, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	o, e := s.NativeRead("skills.package", "demo", 1)
	if e != nil || len(o.Package.Files) != 16 {
		t.Fatal(e)
	}
}
