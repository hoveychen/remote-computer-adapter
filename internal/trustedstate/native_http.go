package trustedstate

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// NativeCredential is configured only by the trusted owner. It binds a token
// to a role and thread; native tool arguments cannot select an actor or domain.
// Background credentials currently have only handshake/read capabilities.
type NativeCredential struct {
	Token    string
	Kind     string
	ThreadID string
}

func (s *Store) nativeIdentity() (string, error) {
	r, e := s.NativeRead("maintenance", "store-id", 0)
	if e == nil {
		return string(r.Content), nil
	}
	if e.Error() != "not_found" {
		return "", e
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return "", e
	}
	id := hex.EncodeToString(b)
	result, e := s.NativeBatch(NativeRequest{RequestID: "native-store-identity", Actor: NativeActor{Kind: "maintenance"}, Operation: "store.initialize", Changes: []NativeChange{{Domain: "maintenance", Key: "store-id", Content: []byte(id)}}})
	if e != nil {
		return "", e
	}
	if result.Error != "" {
		return "", errors.New(result.Error)
	}
	return id, nil
}
func NativeHTTPHandler(s *Store, credentials []NativeCredential) (http.Handler, error) {
	creds := append([]NativeCredential(nil), credentials...)
	seen := map[string]bool{}
	if len(creds) == 0 {
		return nil, errors.New("native credentials required")
	}
	for _, c := range creds {
		if len(c.Token) < 32 || seen[c.Token] || !logicalID.MatchString(c.ThreadID) || (c.Kind != "model_tool" && c.Kind != "background" && c.Kind != "installer") {
			return nil, errors.New("invalid native credential")
		}
		seen[c.Token] = true
	}
	identity, e := s.nativeIdentity()
	if e != nil {
		return nil, e
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var subject *NativeCredential
		for i := range creds {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+creds[i].Token)) == 1 {
				subject = &creds[i]
			}
		}
		if subject == nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "origin not allowed", 403)
			return
		}
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST required", 405)
			return
		}
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 2*NativeResourceBytes))
		if e != nil {
			http.Error(w, "request too large", 413)
			return
		}
		decode := func(v any) bool {
			if len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' || strictJSON(body, v) != nil {
				http.Error(w, "invalid JSON request", 400)
				return false
			}
			return true
		}
		var result any
		requireBackground := func() bool {
			if subject.Kind != "background" {
				http.Error(w, "role not authorized", 403)
				return false
			}
			return true
		}
		setReceipt := func(receipt NativeResult, err error) {
			e = err
			if e == nil && receipt.Error != "" {
				e = errors.New(receipt.Error)
			}
			if e == nil {
				w.Header().Set("X-RCA-Commit-Sequence", jsonNumber(receipt.CommitSequence))
				result = receipt
			}
		}
		switch r.URL.Path {
		case "/native/v2/handshake":
			var q struct{}
			if !decode(&q) {
				return
			}
			if _, e = s.NativeRead("maintenance", "store-id", 0); e == nil {
				caps := []string{"native_memory_read"}
				if subject.Kind == "model_tool" {
					caps = append(caps, "native_memory_note")
				} else if subject.Kind == "background" {
					caps = append(caps, "native_memory_jobs", "native_memory_consolidation")
				}
				result = map[string]any{"protocol": 2, "store_id": identity, "capabilities": caps, "limits": map[string]int{"resource_bytes": NativeResourceBytes, "page_bytes": NativePageBytes}}
			}
		case "/native/v2/memory.note.create":
			if subject.Kind != "model_tool" {
				http.Error(w, "role not authorized", 403)
				return
			}
			var q struct {
				CallID   string `json:"call_id"`
				Filename string `json:"filename"`
				Note     string `json:"note"`
			}
			if !decode(&q) {
				return
			}
			var receipt NativeResult
			receipt, e = s.MemoryNote(NativeActor{Kind: subject.Kind, ThreadID: subject.ThreadID}, q.CallID, q.Filename, q.Note)
			if e == nil && receipt.Error != "" {
				e = errors.New(receipt.Error)
			}
			if e == nil {
				w.Header().Set("X-RCA-Commit-Sequence", jsonNumber(receipt.CommitSequence))
				result = struct{}{}
			}
		case "/native/v2/memory.list":
			var q MemoryListRequest
			if !decode(&q) {
				return
			}
			result, e = s.MemoryList(q)
		case "/native/v2/memory.read":
			var q MemoryReadRequest
			if !decode(&q) {
				return
			}
			result, e = s.MemoryRead(q)
		case "/native/v2/memory.search":
			var q MemorySearchRequest
			if !decode(&q) {
				return
			}
			result, e = s.MemorySearch(q)
		case "/native/v2/memory.summary.read":
			var q struct{}
			if !decode(&q) {
				return
			}
			result, e = s.MemoryRead(MemoryReadRequest{Path: "memory_summary.md", LineOffset: 1})
		case "/native/v2/memory.stage1.enqueue":
			if !requireBackground() {
				return
			}
			var q struct {
				RequestID    string `json:"request_id"`
				JobID        string `json:"job_id"`
				InputVersion string `json:"input_version"`
			}
			if !decode(&q) {
				return
			}
			setReceipt(s.MemoryStage1Enqueue(q.RequestID, q.JobID, q.InputVersion))
		case "/native/v2/memory.job.claim":
			if !requireBackground() {
				return
			}
			var q struct {
				RequestID    string `json:"request_id"`
				JobID        string `json:"job_id"`
				LeaseSeconds uint32 `json:"lease_seconds"`
			}
			if !decode(&q) {
				return
			}
			setReceipt(s.MemoryJobClaim(q.RequestID, q.JobID, q.LeaseSeconds))
		case "/native/v2/memory.job.heartbeat":
			if !requireBackground() {
				return
			}
			var q struct {
				RequestID    string `json:"request_id"`
				JobID        string `json:"job_id"`
				LeaseToken   string `json:"lease_token"`
				LeaseSeconds uint32 `json:"lease_seconds"`
			}
			if !decode(&q) {
				return
			}
			setReceipt(s.MemoryJobHeartbeat(q.RequestID, q.JobID, q.LeaseToken, q.LeaseSeconds))
		case "/native/v2/memory.job.fail":
			if !requireBackground() {
				return
			}
			var q struct {
				RequestID  string `json:"request_id"`
				JobID      string `json:"job_id"`
				LeaseToken string `json:"lease_token"`
				Reason     string `json:"reason"`
			}
			if !decode(&q) {
				return
			}
			setReceipt(s.MemoryJobFail(q.RequestID, q.JobID, q.LeaseToken, q.Reason))
		case "/native/v2/memory.stage1.commit":
			if !requireBackground() {
				return
			}
			var q struct {
				RequestID      string `json:"request_id"`
				JobID          string `json:"job_id"`
				LeaseToken     string `json:"lease_token"`
				RawMemory      string `json:"raw_memory"`
				RolloutSummary string `json:"rollout_summary"`
			}
			if !decode(&q) {
				return
			}
			setReceipt(s.MemoryStage1Commit(q.RequestID, q.JobID, q.LeaseToken, q.RawMemory, q.RolloutSummary))
		case "/native/v2/memory.phase2.begin":
			if !requireBackground() {
				return
			}
			var q struct {
				RequestID    string `json:"request_id"`
				LeaseSeconds uint32 `json:"lease_seconds"`
			}
			if !decode(&q) {
				return
			}
			setReceipt(s.MemoryPhase2Begin(q.RequestID, q.LeaseSeconds))
		case "/native/v2/memory.phase2.commit":
			if !requireBackground() {
				return
			}
			var q struct {
				RequestID     string `json:"request_id"`
				LeaseToken    string `json:"lease_token"`
				TransactionID string `json:"transaction_id"`
				Memory        string `json:"memory"`
				Summary       string `json:"summary"`
			}
			if !decode(&q) {
				return
			}
			setReceipt(s.MemoryPhase2Commit(q.RequestID, q.LeaseToken, q.TransactionID, q.Memory, q.Summary))
		case "/native/v2/memory.projection":
			if !requireBackground() {
				return
			}
			var q struct{}
			if !decode(&q) {
				return
			}
			result, e = s.MemoryProjection()
		default:
			http.NotFound(w, r)
			return
		}
		if e != nil {
			status := 400
			if strings.Contains(e.Error(), "unavailable") || strings.Contains(e.Error(), "persistence failed") {
				status = 503
			} else if e.Error() == "snapshot_expired" || e.Error() == "revision_conflict" || e.Error() == "lease_lost" || e.Error() == "lease_unavailable" || e.Error() == "stale_consolidation" || e.Error() == "job_conflict" {
				status = 409
			} else if e.Error() == "not_found" {
				status = 404
			}
			http.Error(w, e.Error(), status)
			return
		}
		switch out := result.(type) {
		case MemoryListResult:
			w.Header().Set("X-RCA-Snapshot", jsonNumber(out.snapshot))
		case MemoryReadResult:
			w.Header().Set("X-RCA-Snapshot", jsonNumber(out.snapshot))
		case MemorySearchResult:
			w.Header().Set("X-RCA-Snapshot", jsonNumber(out.snapshot))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}), nil
}
func jsonNumber(v uint64) string { b, _ := json.Marshal(v); return string(b) }
