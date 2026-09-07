// Package trustedstate provides a small, journal-backed semantic state store.
// The trusted owner supplies the root; tool callers only supply logical IDs.
package trustedstate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const MaxContent = 256 * 1024

var logicalID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

type Request struct {
	ID               string `json:"id"`
	Content          string `json:"content,omitempty"`
	ExpectedRevision uint64 `json:"expected_revision"`
	RequestID        string `json:"request_id"`
}
type Object struct {
	ID       string `json:"id"`
	Content  string `json:"content,omitempty"`
	Revision uint64 `json:"revision"`
	Deleted  bool   `json:"deleted,omitempty"`
}
type Result struct {
	Object Object `json:"object"`
	Error  string `json:"error,omitempty"`
}
type record struct {
	Sequence   uint64  `json:"sequence"`
	Time       string  `json:"time"`
	Collection string  `json:"collection"`
	Operation  string  `json:"operation"`
	Request    Request `json:"request"`
	Before     uint64  `json:"before_revision"`
	Result     Result  `json:"result"`
}
type journal interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}
type Store struct {
	mu               sync.Mutex
	file             journal
	objects          map[string]Object
	requests         map[string]record
	native           map[string]NativeResource
	nativeRequests   map[string]nativeRecord
	nativeJobs       map[string]NativeJob
	memoryGeneration uint64
	sequence         uint64
	poisoned         bool
}

// Open refuses corrupt/incomplete journals rather than silently losing audit
// entries. A failed fsync has an uncertain outcome: reopen and retry the SAME
// request_id. The persisted journal, if intact, resolves that uncertainty.
func Open(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("state root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state root must be a private directory (0700)")
	}
	fd, err := unix.Open(filepath.Join(root, "journal.jsonl"), unix.O_RDWR|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "state journal")
	fail := func(err error) (*Store, error) { f.Close(); return nil, err }
	fi, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0077 != 0 {
		return fail(errors.New("journal must be a private regular file"))
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("state already in use: %w", err))
	}
	s := &Store{
		file:           f,
		objects:        map[string]Object{},
		requests:       map[string]record{},
		native:         map[string]NativeResource{},
		nativeRequests: map[string]nativeRecord{},
		nativeJobs:     map[string]NativeJob{},
	}
	reader := bufio.NewScanner(f)
	reader.Buffer(make([]byte, 4096), NativeTransactionBytes+1)
	// Preserve newline presence: a valid JSON object without its final newline
	// is still an incomplete transaction, not a recoverable committed record.
	reader.Split(journalLines)
	for reader.Scan() {
		line := reader.Bytes()
		var envelope struct {
			FormatVersion int `json:"format_version"`
		}
		if err = json.Unmarshal(line, &envelope); err != nil {
			return fail(err)
		}
		if envelope.FormatVersion == 2 {
			if err = s.replayNative(line); err != nil {
				return fail(err)
			}
			continue
		}
		if envelope.FormatVersion != 0 && envelope.FormatVersion != 1 {
			return fail(errors.New("unknown journal version"))
		}

		var r record
		if err = json.Unmarshal(line, &r); err != nil {
			return fail(fmt.Errorf("corrupt audit journal: %w", err))
		}
		if err = validate(r.Collection, r.Operation, r.Request); err != nil {
			return fail(err)
		}
		if _, exists := s.nativeRequests[r.Request.RequestID]; exists {
			return fail(errors.New("duplicate journal request"))
		}
		if _, exists := s.requests[r.Request.RequestID]; exists {
			return fail(errors.New("duplicate journal request"))
		}
		before, result := s.evaluate(r.Collection, r.Operation, r.Request)
		if r.Sequence != s.sequence+1 || before != r.Before || !reflect.DeepEqual(result, r.Result) {
			return fail(errors.New("inconsistent audit journal"))
		}
		s.apply(r)
	}
	if err = reader.Err(); err != nil {
		return fail(err)
	}
	// Persist directory entry as well as file contents before accepting mutations.
	d, err := os.Open(root)
	if err != nil {
		return fail(err)
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return fail(err)
	}
	return s, nil
}
func validate(collection, operation string, q Request) error {
	if collection != "memory" && collection != "skills" {
		return errors.New("unknown collection")
	}
	if operation != "put" && operation != "delete" {
		return errors.New("unknown mutation")
	}
	if !logicalID.MatchString(q.ID) || !logicalID.MatchString(q.RequestID) {
		return errors.New("invalid logical id or request_id")
	}
	if len(q.Content) > MaxContent {
		return errors.New("content too large")
	}
	if operation == "delete" && q.Content != "" {
		return errors.New("delete does not accept content")
	}
	return nil
}
func (s *Store) evaluate(collection, op string, q Request) (uint64, Result) {
	o, exists := s.objects[collection+"/"+q.ID]
	if !exists {
		o.ID = q.ID
	}
	r := Result{Object: o}
	if alias, ok := s.native["migration/legacy/"+collection+"/"+q.ID]; ok && !alias.Deleted {
		r.Error = "migrated_read_only"
		return o.Revision, r
	}
	if o.Revision != q.ExpectedRevision {
		r.Error = "revision_conflict"
		return o.Revision, r
	}
	if op == "delete" && (!exists || o.Deleted) {
		r.Error = "not_found"
		return o.Revision, r
	}
	if o.Revision == ^uint64(0) {
		r.Error = "revision_exhausted"
		return o.Revision, r
	}
	r.Object = Object{ID: q.ID, Revision: o.Revision + 1, Content: q.Content, Deleted: op == "delete"}
	return o.Revision, r
}
func (s *Store) apply(r record) {
	if r.Result.Error == "" {
		s.objects[r.Collection+"/"+r.Request.ID] = r.Result.Object
	}
	s.requests[r.Request.RequestID] = r
	s.sequence = r.Sequence
}
func (s *Store) Mutate(collection, op string, q Request) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return Result{}, errors.New("state unavailable; reopen and reconcile journal")
	}
	if err := validate(collection, op, q); err != nil {
		return Result{}, err
	}
	if _, ok := s.nativeRequests[q.RequestID]; ok {
		return Result{}, errors.New("request_id reused across protocols")
	}
	if r, ok := s.requests[q.RequestID]; ok {
		if r.Collection != collection || r.Operation != op || r.Request != q {
			return Result{}, errors.New("request_id reused with different arguments")
		}
		return r.Result, nil
	}
	before, result := s.evaluate(collection, op, q)
	r := record{Sequence: s.sequence + 1, Time: time.Now().UTC().Format(time.RFC3339Nano), Collection: collection, Operation: op, Request: q, Before: before, Result: result}
	if err := s.appendRecord(r); err != nil {
		return Result{}, err
	}
	s.apply(r)
	return result, nil
}
func (s *Store) Read(collection, id string) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return Object{}, errors.New("state unavailable")
	}
	if (collection != "memory" && collection != "skills") || !logicalID.MatchString(id) {
		return Object{}, errors.New("invalid collection or id")
	}
	o, ok := s.objects[collection+"/"+id]
	if !ok {
		return Object{}, errors.New("not_found")
	}
	return o, nil
}

// List includes tombstones, so clients can recreate a deleted ID using CAS.
func (s *Store) List(collection string) ([]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, errors.New("state unavailable")
	}
	if collection != "memory" && collection != "skills" {
		return nil, errors.New("invalid collection")
	}
	out := []Object{}
	for key, o := range s.objects {
		if key == collection+"/"+o.ID {
			o.Content = ""
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.poisoned = true
	return s.file.Close()
}
