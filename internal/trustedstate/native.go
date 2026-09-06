package trustedstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	NativeResourceBytes      = 1 << 20
	NativePackageBytes       = 16 << 20
	NativePackageFiles       = 512
	NativeTransactionBytes   = 32 << 20
	NativeTransactionChanges = 1024
)

type NativeActor struct {
	Kind     string `json:"kind"`
	ThreadID string `json:"thread_id,omitempty"`
	CallID   string `json:"call_id,omitempty"`
}
type NativeFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content_base64"`
	SHA256  string `json:"sha256"`
}

// Packages contain only content. MCP, hooks and credentials are never inferred
// from a manifest. Authority is assigned by the trusted semantic adapter.
type NativePackage struct {
	Authority string       `json:"authority"`
	Enabled   bool         `json:"enabled"`
	Files     []NativeFile `json:"files"`
}
type NativeChange struct {
	Domain           string         `json:"domain"`
	Key              string         `json:"key"`
	ExpectedRevision uint64         `json:"expected_revision"`
	Content          []byte         `json:"content_base64,omitempty"`
	Package          *NativePackage `json:"package,omitempty"`
	Deleted          bool           `json:"deleted,omitempty"`
}
type NativeRequest struct {
	RequestID string         `json:"request_id"`
	Actor     NativeActor    `json:"actor"`
	Operation string         `json:"operation"`
	Changes   []NativeChange `json:"changes"`
}
type NativeResource struct {
	Domain   string         `json:"domain"`
	Key      string         `json:"key"`
	Revision uint64         `json:"revision"`
	Content  []byte         `json:"content_base64,omitempty"`
	SHA256   string         `json:"sha256"`
	Package  *NativePackage `json:"package,omitempty"`
	Deleted  bool           `json:"deleted,omitempty"`
}
type NativeResult struct {
	Status         string           `json:"status"`
	CommitSequence uint64           `json:"commit_sequence"`
	Error          string           `json:"error,omitempty"`
	Resources      []NativeResource `json:"resources,omitempty"`
}
type nativeRecord struct {
	FormatVersion int           `json:"format_version"`
	Kind          string        `json:"kind"`
	Sequence      uint64        `json:"sequence"`
	Time          string        `json:"time"`
	Request       NativeRequest `json:"request"`
	RequestDigest string        `json:"request_digest"`
	Result        NativeResult  `json:"result"`
}

func hashBytes(b []byte) string           { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func nativeKey(domain, key string) string { return domain + "/" + key }
func validNativePath(p string) bool {
	if p == "" || len(p) > 1024 || !utf8.ValidString(p) || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, c := range p {
		if c < 32 || c == 127 || c == '\\' || c == ':' {
			return false
		}
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return true
}
func validNativeDomain(d string) bool {
	switch d {
	case "memory.note", "memory.stage1", "memory.artifact", "memory.extension", "skills.package", "maintenance", "migration":
		return true
	}
	return false
}

// normalizeNative owns a deep copy, computes all hashes and establishes stable
// ordering before digesting. Neither callers nor read results alias store state.
var nativeOperation = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,127}$`)

func normalizeNative(q NativeRequest) (NativeRequest, string, error) {
	if !logicalID.MatchString(q.RequestID) || !nativeOperation.MatchString(q.Operation) {
		return q, "", errors.New("invalid request identity or operation")
	}
	switch q.Actor.Kind {
	case "model_tool", "background", "installer", "maintenance", "migration":
	default:
		return q, "", errors.New("invalid actor")
	}
	if len(q.Actor.ThreadID) > 128 || len(q.Actor.CallID) > 128 {
		return q, "", errors.New("actor identity too long")
	}
	if len(q.Changes) == 0 || len(q.Changes) > NativeTransactionChanges {
		return q, "", errors.New("invalid change count")
	}
	total := 0
	seen := map[string]bool{}
	for _, c := range q.Changes {
		k := nativeKey(c.Domain, c.Key)
		if !validNativeDomain(c.Domain) || !validNativePath(c.Key) || seen[k] {
			return q, "", errors.New("invalid or duplicate resource")
		}
		seen[k] = true
		if len(c.Content) > NativeResourceBytes {
			return q, "", errors.New("resource too large")
		}
		total += len(c.Content)
		if c.Deleted && (len(c.Content) != 0 || c.Package != nil) {
			return q, "", errors.New("delete has content")
		}
		if c.Domain == "skills.package" && !c.Deleted {
			if c.Package == nil || len(c.Content) != 0 {
				return q, "", errors.New("package manifest required")
			}
			p := c.Package
			if !logicalID.MatchString(p.Authority) || len(p.Files) == 0 || len(p.Files) > NativePackageFiles {
				return q, "", errors.New("invalid package authority or count")
			}
			files := map[string]bool{}
			size := 0
			for _, f := range p.Files {
				if !validNativePath(f.Path) || files[f.Path] || len(f.Content) > NativeResourceBytes {
					return q, "", errors.New("invalid package file")
				}
				files[f.Path] = true
				size += len(f.Content)
			}
			if !files["SKILL.md"] || size > NativePackageBytes {
				return q, "", errors.New("invalid package size or missing SKILL.md")
			}
			total += size
		} else if c.Package != nil {
			return q, "", errors.New("manifest outside package domain")
		}
		if total > NativeTransactionBytes {
			return q, "", errors.New("transaction too large")
		}
	}
	// Raw content is bounded before JSON encoding to bound temporary allocations.
	data, err := json.Marshal(q)
	if err != nil || len(data) > NativeTransactionBytes {
		return q, "", errors.New("transaction encoding too large")
	}
	var out NativeRequest
	if err = json.Unmarshal(data, &out); err != nil {
		return q, "", err
	}
	for i := range out.Changes {
		if p := out.Changes[i].Package; p != nil {
			for j := range p.Files {
				p.Files[j].SHA256 = hashBytes(p.Files[j].Content)
			}
			sort.Slice(p.Files, func(a, b int) bool { return p.Files[a].Path < p.Files[b].Path })
		}
	}
	sort.Slice(out.Changes, func(i, j int) bool {
		return nativeKey(out.Changes[i].Domain, out.Changes[i].Key) < nativeKey(out.Changes[j].Domain, out.Changes[j].Key)
	})
	data, err = json.Marshal(out)
	if err != nil {
		return q, "", err
	}
	return out, hashBytes(data), nil
}
func (s *Store) evaluateNative(q NativeRequest) NativeResult {
	result := NativeResult{Status: "rejected", CommitSequence: s.sequence + 1}
	if reason := s.validateLegacyAlias(q); reason != "" {
		result.Error = reason
		return result
	}
	for _, c := range q.Changes {
		old := s.native[nativeKey(c.Domain, c.Key)]
		if old.Revision != c.ExpectedRevision {
			result.Error = "revision_conflict"
			return result
		}
		if old.Revision == ^uint64(0) {
			result.Error = "revision_exhausted"
			return result
		}
		if c.Deleted && (old.Revision == 0 || old.Deleted) {
			result.Error = "not_found"
			return result
		}
	}
	if !s.validMemoryPaths(q) {
		result.Error = "memory_path_conflict"
		return result
	}
	result.Status = "committed"
	for _, c := range q.Changes {
		r := NativeResource{Domain: c.Domain, Key: c.Key, Revision: c.ExpectedRevision + 1, Content: c.Content, Package: c.Package, Deleted: c.Deleted, SHA256: hashBytes(c.Content)}
		if c.Package != nil {
			b, _ := json.Marshal(c.Package)
			r.SHA256 = hashBytes(b)
		}
		result.Resources = append(result.Resources, r)
	}
	return result
}
func (s *Store) applyNative(r nativeRecord) {
	if r.Result.Error == "" {
		for _, o := range r.Result.Resources {
			s.native[nativeKey(o.Domain, o.Key)] = o
		}
	}
	s.nativeRequests[r.Request.RequestID] = r
	s.sequence = r.Sequence
}
func cloneNativeResult(r NativeResult) NativeResult {
	b, _ := json.Marshal(r)
	var out NativeResult
	_ = json.Unmarshal(b, &out)
	return out
}

// NativeBatch is an internal primitive, never a model-facing filesystem tool.
func (s *Store) NativeBatch(q NativeRequest) (NativeResult, error) {
	q, digest, err := normalizeNative(q)
	if err != nil {
		return NativeResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return NativeResult{}, errors.New("state unavailable")
	}
	if _, ok := s.requests[q.RequestID]; ok {
		return NativeResult{}, errors.New("request_id reused across protocols")
	}
	if r, ok := s.nativeRequests[q.RequestID]; ok {
		if r.RequestDigest != digest {
			return NativeResult{}, errors.New("request_id reused with different arguments")
		}
		return cloneNativeResult(r.Result), nil
	}
	result := s.evaluateNative(q)
	r := nativeRecord{FormatVersion: 2, Kind: "native_transaction", Sequence: s.sequence + 1, Time: time.Now().UTC().Format(time.RFC3339Nano), Request: q, RequestDigest: digest, Result: result}
	// The request already holds all replayable bytes. Persist the receipt
	// without duplicating content; reconstruct resources during replay.
	durable := r
	durable.Result.Resources = nil
	if err = s.appendRecord(durable); err != nil {
		return NativeResult{}, err
	}
	s.applyNative(r)
	return cloneNativeResult(result), nil
}
func (s *Store) NativeRead(domain, key string, revision uint64) (NativeResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return NativeResource{}, errors.New("state unavailable")
	}
	if !validNativeDomain(domain) || !validNativePath(key) {
		return NativeResource{}, errors.New("invalid resource")
	}
	r, ok := s.native[nativeKey(domain, key)]
	if !ok {
		return NativeResource{}, errors.New("not_found")
	}
	// Old snapshots explicitly expire rather than silently returning new bytes.
	if revision != 0 && r.Revision != revision {
		return NativeResource{}, errors.New("snapshot_expired")
	}
	return cloneNativeResult(NativeResult{Resources: []NativeResource{r}}).Resources[0], nil
}
func (s *Store) appendRecord(r any) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b)+1 > NativeTransactionBytes {
		return errors.New("journal entry too large")
	}
	b = append(b, '\n')
	n, err := s.file.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.file.Sync()
	}
	if err != nil {
		s.poisoned = true
		return fmt.Errorf("audit persistence failed; outcome uncertain: %w", err)
	}
	return nil
}
func (s *Store) replayNative(line []byte) error {
	var r nativeRecord
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return err
	}
	if r.FormatVersion != 2 || r.Kind != "native_transaction" || r.Sequence != s.sequence+1 {
		return errors.New("inconsistent native envelope")
	}
	if _, ok := s.requests[r.Request.RequestID]; ok {
		return errors.New("duplicate journal request")
	}
	if _, ok := s.nativeRequests[r.Request.RequestID]; ok {
		return errors.New("duplicate journal request")
	}
	q, digest, err := normalizeNative(r.Request)
	if err != nil {
		return err
	}
	expected := s.evaluateNative(q)
	receipt := expected
	receipt.Resources = nil
	if digest != r.RequestDigest || !reflect.DeepEqual(q, r.Request) || !reflect.DeepEqual(receipt, r.Result) {
		return errors.New("inconsistent native transaction")
	}
	r.Result = expected
	s.applyNative(r)
	return nil
}

func journalLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		if i+1 > NativeTransactionBytes {
			return 0, nil, errors.New("oversize audit entry")
		}
		return i + 1, data[:i], nil
	}
	if len(data) >= NativeTransactionBytes {
		return 0, nil, errors.New("oversize audit entry")
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("incomplete audit journal")
	}
	return 0, nil, nil
}
