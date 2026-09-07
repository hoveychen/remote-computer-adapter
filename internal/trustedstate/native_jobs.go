package trustedstate

import (
	"encoding/json"
	"errors"
	"sort"
)

type MemoryProjection struct {
	CommitSequence   uint64           `json:"commit_sequence"`
	MemoryGeneration uint64           `json:"memory_generation"`
	Jobs             []NativeJob      `json:"jobs"`
	Resources        []NativeResource `json:"resources,omitempty"`
}

func backgroundRequest(requestID, operation string, cmd NativeJobCommand, changes ...NativeChange) NativeRequest {
	return NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "background"}, Operation: operation, Changes: changes, Job: &cmd}
}

func (s *Store) MemoryStage1Enqueue(requestID, jobID, inputVersion string) (NativeResult, error) {
	if inputVersion == "" {
		return NativeResult{}, errors.New("input fingerprint required")
	}
	serverVersion := "sha256:" + hashBytes([]byte(inputVersion))
	return s.NativeBatch(backgroundRequest(requestID, "memory.stage1.enqueue", NativeJobCommand{JobID: jobID, Kind: "stage1", InputVersion: serverVersion}))
}

func (s *Store) MemoryJobClaim(requestID, jobID string, leaseSeconds uint32) (NativeResult, error) {
	return s.NativeBatch(backgroundRequest(requestID, "memory.job.claim", NativeJobCommand{JobID: jobID, LeaseSeconds: leaseSeconds}))
}

func (s *Store) MemoryJobHeartbeat(requestID, jobID, leaseToken string, leaseSeconds uint32) (NativeResult, error) {
	return s.NativeBatch(backgroundRequest(requestID, "memory.job.heartbeat", NativeJobCommand{JobID: jobID, LeaseToken: leaseToken, LeaseSeconds: leaseSeconds}))
}

func (s *Store) MemoryJobFail(requestID, jobID, leaseToken, reason string) (NativeResult, error) {
	return s.NativeBatch(backgroundRequest(requestID, "memory.job.fail", NativeJobCommand{JobID: jobID, LeaseToken: leaseToken, Failure: reason}))
}

func (s *Store) MemoryStage1Commit(requestID, jobID, leaseToken, rawMemory, rolloutSummary string) (NativeResult, error) {
	rawRevision := s.nativeRevision("memory.stage1", "raw/"+jobID+".md")
	summaryRevision := s.nativeRevision("memory.stage1", "summary/"+jobID+".md")
	return s.NativeBatch(backgroundRequest(requestID, "memory.stage1.commit", NativeJobCommand{JobID: jobID, LeaseToken: leaseToken},
		NativeChange{Domain: "memory.stage1", Key: "raw/" + jobID + ".md", ExpectedRevision: rawRevision, Content: []byte(rawMemory)},
		NativeChange{Domain: "memory.stage1", Key: "summary/" + jobID + ".md", ExpectedRevision: summaryRevision, Content: []byte(rolloutSummary)}))
}

func (s *Store) MemoryPhase2Begin(requestID string, leaseSeconds, maxInputs uint32) (NativeResult, error) {
	return s.NativeBatch(backgroundRequest(requestID, "memory.phase2.begin", NativeJobCommand{JobID: "global", LeaseSeconds: leaseSeconds, MaxInputs: maxInputs}))
}

func (s *Store) nativeRevision(domain, key string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.native[nativeKey(domain, key)].Revision
}

func (s *Store) MemoryPhase2Commit(requestID, leaseToken, transactionID, memory, summary string) (NativeResult, error) {
	memoryRevision := s.nativeRevision("memory.artifact", "MEMORY.md")
	summaryRevision := s.nativeRevision("memory.artifact", "memory_summary.md")
	return s.NativeBatch(backgroundRequest(requestID, "memory.phase2.commit", NativeJobCommand{JobID: "global", LeaseToken: leaseToken, TransactionID: transactionID},
		NativeChange{Domain: "memory.artifact", Key: "MEMORY.md", ExpectedRevision: memoryRevision, Content: []byte(memory)},
		NativeChange{Domain: "memory.artifact", Key: "memory_summary.md", ExpectedRevision: summaryRevision, Content: []byte(summary)}))
}

func (s *Store) MemoryRecordUsage(requestID, domain, key string, revision uint64) (NativeResult, error) {
	resource, err := s.NativeRead(domain, key, revision)
	if err != nil {
		return NativeResult{}, err
	}
	body, _ := json.Marshal(struct {
		Domain   string `json:"domain"`
		Key      string `json:"key"`
		Revision uint64 `json:"revision"`
		SHA256   string `json:"sha256"`
	}{resource.Domain, resource.Key, resource.Revision, resource.SHA256})
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "background"}, Operation: "memory.usage.record", Changes: []NativeChange{{Domain: "maintenance", Key: "memory-usage/" + requestID, Content: body}}})
}

func (s *Store) memoryDeleteTransaction(requestID, operation string, predicate func(NativeResource) bool) (NativeResult, error) {
	s.mu.Lock()
	changes := make([]NativeChange, 0)
	for _, resource := range s.native {
		if !resource.Deleted && memoryDomain(resource.Domain) && predicate(resource) {
			changes = append(changes, NativeChange{Domain: resource.Domain, Key: resource.Key, ExpectedRevision: resource.Revision, Deleted: true})
		}
	}
	s.mu.Unlock()
	sort.Slice(changes, func(i, j int) bool {
		return nativeKey(changes[i].Domain, changes[i].Key) < nativeKey(changes[j].Domain, changes[j].Key)
	})
	receipt, _ := json.Marshal(struct {
		Deleted int `json:"deleted"`
	}{len(changes)})
	changes = append(changes, NativeChange{Domain: "maintenance", Key: operation + "/" + requestID, Content: receipt})
	if len(changes) > NativeTransactionChanges {
		return NativeResult{}, errors.New("too many resources for one maintenance transaction")
	}
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "background"}, Operation: operation, Changes: changes})
}

func (s *Store) MemoryRetainStage1(requestID string, keep map[string]bool) (NativeResult, error) {
	return s.memoryDeleteTransaction(requestID, "memory.retention", func(resource NativeResource) bool {
		return resource.Domain == "memory.stage1" && !keep[resource.Key]
	})
}

func (s *Store) MemoryClear(requestID string) (NativeResult, error) {
	return s.memoryDeleteTransaction(requestID, "memory.clear", func(NativeResource) bool { return true })
}

func (s *Store) MemoryProjection() (MemoryProjection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return MemoryProjection{}, errors.New("state unavailable")
	}
	out := MemoryProjection{CommitSequence: s.sequence, MemoryGeneration: s.memoryGeneration}
	for _, job := range s.nativeJobs {
		out.Jobs = append(out.Jobs, cloneNativeJob(job))
	}
	for _, resource := range s.native {
		if memoryDomain(resource.Domain) {
			out.Resources = append(out.Resources, cloneNativeResult(NativeResult{Resources: []NativeResource{resource}}).Resources[0])
		}
	}
	sort.Slice(out.Jobs, func(i, j int) bool { return out.Jobs[i].JobID < out.Jobs[j].JobID })
	sort.Slice(out.Resources, func(i, j int) bool {
		return nativeKey(out.Resources[i].Domain, out.Resources[i].Key) < nativeKey(out.Resources[j].Domain, out.Resources[j].Key)
	})
	return out, nil
}
