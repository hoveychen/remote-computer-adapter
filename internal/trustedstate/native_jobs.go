package trustedstate

import (
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type memoryUsage struct {
	Count       uint64 `json:"count"`
	LastUsageAt int64  `json:"last_usage_at"`
}

type MemoryProjection struct {
	CommitSequence   uint64           `json:"commit_sequence"`
	MemoryGeneration uint64           `json:"memory_generation"`
	Jobs             []NativeJob      `json:"jobs"`
	Resources        []NativeResource `json:"resources,omitempty"`
}

func backgroundRequest(requestID, operation string, cmd NativeJobCommand, changes ...NativeChange) NativeRequest {
	return NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "background"}, Operation: operation, Changes: changes, Job: &cmd}
}

func nativeIntent(value any) string {
	body, _ := json.Marshal(value)
	return "sha256:" + hashBytes(body)
}

func (s *Store) MemoryStage1Enqueue(requestID, jobID, inputVersion string, inputWatermark int64) (NativeResult, error) {
	if inputVersion == "" {
		return NativeResult{}, errors.New("input fingerprint required")
	}
	serverVersion := "sha256:" + hashBytes([]byte(inputVersion))
	return s.NativeBatch(backgroundRequest(requestID, "memory.stage1.enqueue", NativeJobCommand{JobID: jobID, Kind: "stage1", InputVersion: serverVersion, InputWatermark: inputWatermark}))
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
	noOutput := rawMemory == "" && rolloutSummary == ""
	s.mu.Lock()
	raw := s.native[nativeKey("memory.stage1", "raw/"+jobID+".md")]
	summary := s.native[nativeKey("memory.stage1", "summary/"+jobID+".md")]
	s.mu.Unlock()
	changes := []NativeChange{}
	if noOutput {
		rawLive, summaryLive := raw.Revision != 0 && !raw.Deleted, summary.Revision != 0 && !summary.Deleted
		if rawLive != summaryLive {
			return NativeResult{}, errors.New("inconsistent stage1 artifacts")
		}
		if rawLive {
			changes = append(changes,
				NativeChange{Domain: "memory.stage1", Key: "raw/" + jobID + ".md", ExpectedRevision: raw.Revision, Deleted: true},
				NativeChange{Domain: "memory.stage1", Key: "summary/" + jobID + ".md", ExpectedRevision: summary.Revision, Deleted: true})
		}
	} else {
		changes = append(changes,
			NativeChange{Domain: "memory.stage1", Key: "raw/" + jobID + ".md", ExpectedRevision: raw.Revision, Content: []byte(rawMemory)},
			NativeChange{Domain: "memory.stage1", Key: "summary/" + jobID + ".md", ExpectedRevision: summary.Revision, Content: []byte(rolloutSummary)})
	}
	request := backgroundRequest(requestID, "memory.stage1.commit", NativeJobCommand{JobID: jobID, LeaseToken: leaseToken, NoOutput: noOutput}, changes...)
	request.Intent = nativeIntent([]string{jobID, leaseToken, rawMemory, rolloutSummary})
	return s.NativeBatch(request)
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
	request := backgroundRequest(requestID, "memory.phase2.commit", NativeJobCommand{JobID: "global", LeaseToken: leaseToken, TransactionID: transactionID},
		NativeChange{Domain: "memory.artifact", Key: "MEMORY.md", ExpectedRevision: memoryRevision, Content: []byte(memory)},
		NativeChange{Domain: "memory.artifact", Key: "memory_summary.md", ExpectedRevision: summaryRevision, Content: []byte(summary)})
	request.Intent = nativeIntent([]string{leaseToken, transactionID, memory, summary})
	return s.NativeBatch(request)
}

func (s *Store) memoryUsage(jobID string) memoryUsage {
	resource := s.native[nativeKey("maintenance", "memory-usage/"+jobID+".json")]
	var usage memoryUsage
	if resource.Revision != 0 && !resource.Deleted {
		_ = json.Unmarshal(resource.Content, &usage)
	}
	return usage
}

func (s *Store) MemoryRecordUsage(requestID string, jobIDs []string) (NativeResult, error) {
	if len(jobIDs) == 0 {
		return NativeResult{}, errors.New("job ids required")
	}
	ids := append([]string(nil), jobIDs...)
	sort.Strings(ids)
	changes := make([]NativeChange, 0, len(ids))
	now := time.Now().UTC().Unix()
	s.mu.Lock()
	for i, id := range ids {
		if id == "" || (i > 0 && id == ids[i-1]) {
			s.mu.Unlock()
			return NativeResult{}, errors.New("invalid job ids")
		}
		job, ok := s.nativeJobs[id]
		if !ok || job.Kind != "stage1" || job.Status != "succeeded" {
			s.mu.Unlock()
			return NativeResult{}, errors.New("stage1 job not found")
		}
		key := "memory-usage/" + id + ".json"
		resource := s.native[nativeKey("maintenance", key)]
		usage := s.memoryUsage(id)
		usage.Count++
		usage.LastUsageAt = now
		body, _ := json.Marshal(usage)
		changes = append(changes, NativeChange{Domain: "maintenance", Key: key, ExpectedRevision: resource.Revision, Content: body})
	}
	s.mu.Unlock()
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "background"}, Operation: "memory.usage.record", Intent: nativeIntent(ids), Changes: changes})
}

func (s *Store) memoryDeleteTransaction(requestID, operation, intent string, predicate func(NativeResource) bool) (NativeResult, error) {
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
	return s.NativeBatch(NativeRequest{RequestID: requestID, Actor: NativeActor{Kind: "background"}, Operation: operation, Intent: intent, Changes: changes})
}

func (s *Store) MemoryRetainStage1(requestID string, maxUnusedDays int64, limit uint32) (NativeResult, error) {
	if maxUnusedDays < 0 || limit == 0 {
		return NativeResult{}, errors.New("invalid retention policy")
	}
	cutoff := time.Now().UTC().Add(-time.Duration(maxUnusedDays) * 24 * time.Hour).Unix()
	type candidate struct {
		id             string
		lastRelevantAt int64
	}
	s.mu.Lock()
	candidates := make([]candidate, 0)
	for id, job := range s.nativeJobs {
		if job.Kind != "stage1" || job.Status != "succeeded" {
			continue
		}
		raw := s.native[nativeKey("memory.stage1", "raw/"+id+".md")]
		summary := s.native[nativeKey("memory.stage1", "summary/"+id+".md")]
		if raw.Revision == 0 || raw.Deleted || summary.Revision == 0 || summary.Deleted {
			continue
		}
		usage := s.memoryUsage(id)
		lastRelevantAt := usage.LastUsageAt
		if lastRelevantAt == 0 {
			lastRelevantAt = job.InputWatermark / 1000
		}
		if lastRelevantAt < cutoff {
			candidates = append(candidates, candidate{id: id, lastRelevantAt: lastRelevantAt})
		}
	}
	s.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].lastRelevantAt != candidates[j].lastRelevantAt {
			return candidates[i].lastRelevantAt < candidates[j].lastRelevantAt
		}
		return candidates[i].id < candidates[j].id
	})
	if uint32(len(candidates)) > limit {
		candidates = candidates[:limit]
	}
	prune := map[string]bool{}
	for _, candidate := range candidates {
		prune[candidate.id] = true
	}
	return s.memoryDeleteTransaction(requestID, "memory.retention", nativeIntent([]any{maxUnusedDays, limit}), func(resource NativeResource) bool {
		if resource.Domain != "memory.stage1" {
			return false
		}
		for id := range prune {
			if resource.Key == "raw/"+id+".md" || resource.Key == "summary/"+id+".md" {
				return true
			}
		}
		return false
	})
}

func (s *Store) MemoryClear(requestID string) (NativeResult, error) {
	return s.memoryDeleteTransaction(requestID, "memory.clear", nativeIntent("clear"), func(NativeResource) bool { return true })
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
