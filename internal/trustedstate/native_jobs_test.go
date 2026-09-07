package trustedstate

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func jobRequest(id, operation string, command NativeJobCommand, changes ...NativeChange) NativeRequest {
	return NativeRequest{
		RequestID: id,
		Actor:     NativeActor{Kind: "background", ThreadID: "worker"},
		Operation: operation,
		Changes:   changes,
		Job:       &command,
	}
}

func committedJob(t *testing.T, s *Store, q NativeRequest) NativeResult {
	t.Helper()
	r := batch(t, s, q)
	if r.Status != "committed" || r.Error != "" {
		t.Fatalf("job transaction rejected: %+v", r)
	}
	return r
}

func TestNativeStage1CommitAndPhase2ArtifactsAreAtomic(t *testing.T) {
	s, root := openTest(t)
	committedJob(t, s, jobRequest("enqueue", "memory.stage1.enqueue", NativeJobCommand{JobID: "rollout1", Kind: "stage1", InputVersion: "sha256:one"}))
	claim := committedJob(t, s, jobRequest("claim", "memory.job.claim", NativeJobCommand{JobID: "rollout1", Kind: "stage1", LeaseSeconds: 60}))
	lease := claim.Jobs[0].LeaseToken
	stage1 := committedJob(t, s, jobRequest(
		"stage1-commit",
		"memory.stage1.commit",
		NativeJobCommand{JobID: "rollout1", LeaseToken: lease},
		NativeChange{Domain: "memory.stage1", Key: "raw/rollout1.md", Content: []byte("raw")},
		NativeChange{Domain: "memory.stage1", Key: "summary/rollout1.md", Content: []byte("summary")},
	))
	if len(stage1.Resources) != 2 || len(stage1.Jobs) != 2 || stage1.Jobs[0].Status != "succeeded" || stage1.Jobs[1].JobID != "global" || stage1.MemoryGeneration != 1 {
		t.Fatalf("stage1 output/job/enqueue not committed together: %+v", stage1)
	}
	begin := committedJob(t, s, jobRequest("phase2-begin", "memory.phase2.begin", NativeJobCommand{JobID: "global", LeaseSeconds: 60}))
	phase2 := begin.Jobs[0]
	finish, err := s.MemoryPhase2Commit("phase2-commit", phase2.LeaseToken, phase2.TransactionID, "memory", "v1\nsummary")
	if err != nil || finish.Status != "committed" {
		t.Fatal(finish, err)
	}
	if len(finish.Resources) != 2 || finish.Jobs[0].Status != "succeeded" || finish.MemoryGeneration != 2 {
		t.Fatalf("phase2 artifacts/job not committed together: %+v", finish)
	}
	finishAgain, err := s.MemoryPhase2Commit("phase2-commit", phase2.LeaseToken, phase2.TransactionID, "memory", "v1\nsummary")
	if err != nil || !reflect.DeepEqual(finish, finishAgain) {
		t.Fatalf("phase2 retry changed receipt: %+v %+v %v", finish, finishAgain, err)
	}
	if _, err = s.MemoryPhase2Commit("phase2-commit", phase2.LeaseToken, phase2.TransactionID, "changed", "v1\nsummary"); err == nil {
		t.Fatal("phase2 request id accepted changed artifacts")
	}
	s.Close()
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, key := range []string{"MEMORY.md", "memory_summary.md"} {
		if _, err = reopened.NativeRead("memory.artifact", key, 1); err != nil {
			t.Fatalf("missing replayed %s: %v", key, err)
		}
	}
	job, err := reopened.NativeJobRead("global")
	if err != nil || job.Status != "succeeded" || reopened.memoryGeneration != 2 {
		t.Fatalf("job/generation did not replay: %+v %v", job, err)
	}
}

func TestNativePhase2RejectsLostLeaseAndChangedInputs(t *testing.T) {
	s, _ := openTest(t)
	committedJob(t, s, jobRequest("enqueue", "memory.stage1.enqueue", NativeJobCommand{JobID: "rollout1", Kind: "stage1", InputVersion: "v1"}))
	claim := committedJob(t, s, jobRequest("claim", "memory.job.claim", NativeJobCommand{JobID: "rollout1", LeaseSeconds: 60})).Jobs[0]
	committedJob(t, s, jobRequest("stage1", "memory.stage1.commit", NativeJobCommand{JobID: "rollout1", LeaseToken: claim.LeaseToken},
		NativeChange{Domain: "memory.stage1", Key: "raw/rollout1.md", Content: []byte("raw")},
		NativeChange{Domain: "memory.stage1", Key: "summary/rollout1.md", Content: []byte("summary")}))
	begin := committedJob(t, s, jobRequest("begin", "memory.phase2.begin", NativeJobCommand{JobID: "global", LeaseSeconds: 60})).Jobs[0]
	changes := []NativeChange{
		{Domain: "memory.artifact", Key: "MEMORY.md", Content: []byte("memory")},
		{Domain: "memory.artifact", Key: "memory_summary.md", Content: []byte("v1\nsummary")},
	}
	wrong := batch(t, s, jobRequest("wrong-lease", "memory.phase2.commit", NativeJobCommand{JobID: "global", LeaseToken: "wrong", TransactionID: begin.TransactionID}, changes...))
	if wrong.Error != "stale_consolidation" || len(wrong.Resources) != 0 {
		t.Fatal(wrong)
	}
	batch(t, s, nativeQ("new-note", note("new.md", "new input", 0)))
	stale := batch(t, s, jobRequest("stale", "memory.phase2.commit", NativeJobCommand{JobID: "global", LeaseToken: begin.LeaseToken, TransactionID: begin.TransactionID}, changes...))
	if stale.Error != "stale_consolidation" || len(stale.Resources) != 0 {
		t.Fatal(stale)
	}
	if _, err := s.NativeRead("memory.artifact", "MEMORY.md", 0); err == nil {
		t.Fatal("stale phase2 partially published MEMORY.md")
	}
}

func TestNativeJobClaimIsConcurrentAndIdempotent(t *testing.T) {
	s, _ := openTest(t)
	committedJob(t, s, jobRequest("enqueue", "memory.stage1.enqueue", NativeJobCommand{JobID: "rollout1", Kind: "stage1", InputVersion: "v1"}))
	var wg sync.WaitGroup
	results := make(chan NativeResult, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.NativeBatch(jobRequest(fmt.Sprintf("claim-%d", i), "memory.job.claim", NativeJobCommand{JobID: "rollout1", LeaseSeconds: 60}))
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			results <- r
		}(i)
	}
	wg.Wait()
	close(results)
	committed := 0
	var winner NativeRequest
	var receipt NativeResult
	for r := range results {
		if r.Status == "committed" {
			committed++
			receipt = r
		} else if r.Error != "lease_unavailable" {
			t.Fatalf("unexpected loser: %+v", r)
		}
	}
	if committed != 1 {
		t.Fatalf("got %d claim winners", committed)
	}
	for i := 0; i < 16; i++ {
		q := jobRequest(fmt.Sprintf("claim-%d", i), "memory.job.claim", NativeJobCommand{JobID: "rollout1", LeaseSeconds: 60})
		r, err := s.NativeBatch(q)
		if err == nil && r.Status == "committed" {
			winner = q
			break
		}
	}
	again, err := s.NativeBatch(winner)
	if err != nil || !reflect.DeepEqual(again, receipt) {
		t.Fatalf("claim retry changed server token: %+v %+v %v", receipt, again, err)
	}
}

func TestNativeMemoryMaintenanceAndProjectionReplay(t *testing.T) {
	s, root := openTest(t)
	for _, id := range []string{"keep", "drop"} {
		committedJob(t, s, jobRequest("enqueue-"+id, "memory.stage1.enqueue", NativeJobCommand{JobID: id, Kind: "stage1", InputVersion: "v1", InputWatermark: 1}))
		claim := committedJob(t, s, jobRequest("claim-"+id, "memory.job.claim", NativeJobCommand{JobID: id, LeaseSeconds: 60})).Jobs[0]
		committedJob(t, s, jobRequest("commit-"+id, "memory.stage1.commit", NativeJobCommand{JobID: id, LeaseToken: claim.LeaseToken},
			NativeChange{Domain: "memory.stage1", Key: "raw/" + id + ".md", Content: []byte("raw " + id)},
			NativeChange{Domain: "memory.stage1", Key: "summary/" + id + ".md", Content: []byte("summary " + id)}))
	}
	batch(t, s, nativeQ("seed-artifact", NativeChange{Domain: "memory.artifact", Key: "MEMORY.md", Content: []byte("memory")}))
	usage, err := s.MemoryRecordUsage("usage1", []string{"keep"})
	if err != nil || usage.Status != "committed" {
		t.Fatal(usage, err)
	}
	usageAgain, err := s.MemoryRecordUsage("usage1", []string{"keep"})
	if err != nil || !reflect.DeepEqual(usage, usageAgain) {
		t.Fatalf("usage retry changed receipt: %+v %+v %v", usage, usageAgain, err)
	}
	if _, err = s.MemoryRecordUsage("usage1", []string{"drop"}); err == nil {
		t.Fatal("usage request id accepted different job ids")
	}
	retained, err := s.MemoryRetainStage1("retain1", 1, 10)
	if err != nil || retained.Status != "committed" {
		t.Fatal(retained, err)
	}
	retainedAgain, err := s.MemoryRetainStage1("retain1", 1, 10)
	if err != nil || !reflect.DeepEqual(retained, retainedAgain) {
		t.Fatalf("retention retry changed receipt: %+v %+v %v", retained, retainedAgain, err)
	}
	if _, err = s.NativeRead("memory.stage1", "raw/drop.md", 0); err != nil {
		t.Fatal("retention tombstone must remain auditable", err)
	}
	committedJob(t, s, jobRequest("enqueue-replay", "memory.stage1.enqueue", NativeJobCommand{JobID: "pending", Kind: "stage1", InputVersion: "v1"}))
	claim := committedJob(t, s, jobRequest("claim-replay", "memory.job.claim", NativeJobCommand{JobID: "pending", LeaseSeconds: 60})).Jobs[0]
	before, err := s.MemoryProjection()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	after, err := s.MemoryProjection()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("projection changed after replay: %+v %+v %v", before, after, err)
	}
	replayed, err := s.NativeJobRead("pending")
	if err != nil || replayed.LeaseToken != claim.LeaseToken || replayed.Status != "leased" {
		t.Fatalf("unfinished lease not recovered: %+v %v", replayed, err)
	}
	cleared, err := s.MemoryClear("clear1")
	if err != nil || cleared.Status != "committed" {
		t.Fatal(cleared, err)
	}
	clearedAgain, err := s.MemoryClear("clear1")
	if err != nil || !reflect.DeepEqual(cleared, clearedAgain) {
		t.Fatalf("clear retry changed receipt: %+v %+v %v", cleared, clearedAgain, err)
	}
	projection, err := s.MemoryProjection()
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range projection.Resources {
		if !resource.Deleted {
			t.Fatalf("clear left live memory resource: %+v", resource)
		}
	}
	if len(projection.Jobs) != 0 {
		t.Fatalf("clear left memory jobs or leases live: %+v", projection.Jobs)
	}
	lost := batch(t, s, jobRequest("commit-after-clear", "memory.stage1.commit", NativeJobCommand{JobID: "pending", LeaseToken: claim.LeaseToken},
		NativeChange{Domain: "memory.stage1", Key: "raw/pending.md", Content: []byte("stale")},
		NativeChange{Domain: "memory.stage1", Key: "summary/pending.md", Content: []byte("stale")}))
	if lost.Status != "rejected" || len(lost.Resources) != 0 {
		t.Fatalf("clear did not invalidate old lease: %+v", lost)
	}
	s.Close()
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	projection, err = s.MemoryProjection()
	if err != nil || len(projection.Jobs) != 0 {
		t.Fatalf("cleared jobs reappeared after replay: %+v %v", projection.Jobs, err)
	}
}

func TestNativeStage1InputVersionRefreshesCanonicalOutput(t *testing.T) {
	s, _ := openTest(t)
	first, err := s.MemoryStage1Enqueue("enqueue-v1", "rollout", "source-v1", 1000)
	if err != nil || first.Status != "committed" {
		t.Fatal(first, err)
	}
	same, err := s.MemoryStage1Enqueue("enqueue-v1-again", "rollout", "source-v1", 1000)
	if err != nil || same.Status != "committed" || same.Jobs[0].Revision != first.Jobs[0].Revision {
		t.Fatal(same, err)
	}
	claim, err := s.MemoryJobClaim("claim-v1", "rollout", 60)
	if err != nil || claim.Status != "committed" {
		t.Fatal(claim, err)
	}
	committed, err := s.MemoryStage1Commit("commit-v1", "rollout", claim.Jobs[0].LeaseToken, "old raw", "old summary")
	if err != nil {
		t.Fatal(err)
	}
	committedAgain, err := s.MemoryStage1Commit("commit-v1", "rollout", claim.Jobs[0].LeaseToken, "old raw", "old summary")
	if err != nil || !reflect.DeepEqual(committed, committedAgain) {
		t.Fatalf("stage1 retry changed receipt: %+v %+v %v", committed, committedAgain, err)
	}
	if _, err = s.MemoryStage1Commit("commit-v1", "rollout", claim.Jobs[0].LeaseToken, "changed", "old summary"); err == nil {
		t.Fatal("stage1 request id accepted changed artifacts")
	}
	refreshed, err := s.MemoryStage1Enqueue("enqueue-v2", "rollout", "source-v2", 2000)
	if err != nil || refreshed.Jobs[0].Status != "queued" {
		t.Fatal(refreshed, err)
	}
	claim, err = s.MemoryJobClaim("claim-v2", "rollout", 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MemoryStage1Commit("commit-v2", "rollout", claim.Jobs[0].LeaseToken, "new raw", "new summary"); err != nil {
		t.Fatal(err)
	}
	raw, err := s.NativeRead("memory.stage1", "raw/rollout.md", 2)
	if err != nil || string(raw.Content) != "new raw" {
		t.Fatal(raw, err)
	}
}

func TestNativeStage1NoOutputDoesNotCreateEmptyInputs(t *testing.T) {
	s, _ := openTest(t)
	first, err := s.MemoryStage1Enqueue("enqueue-empty", "empty", "v1", 1000)
	if err != nil || first.Status != "committed" {
		t.Fatal(first, err)
	}
	claim := committedJob(t, s, jobRequest("claim-empty", "memory.job.claim", NativeJobCommand{JobID: "empty", LeaseSeconds: 60})).Jobs[0]
	result, err := s.MemoryStage1Commit("commit-empty", "empty", claim.LeaseToken, "", "")
	if err != nil || result.Status != "committed" || len(result.Resources) != 0 || len(result.Jobs) != 1 {
		t.Fatalf("no-output created resources or phase2 job: %+v %v", result, err)
	}
	if _, err = s.NativeJobRead("global"); err == nil {
		t.Fatal("no-output without old artifacts queued phase2")
	}

	refresh, err := s.MemoryStage1Enqueue("enqueue-output", "empty", "v2", 2000)
	if err != nil || refresh.Status != "committed" {
		t.Fatal(refresh, err)
	}
	claim = committedJob(t, s, jobRequest("claim-output", "memory.job.claim", NativeJobCommand{JobID: "empty", LeaseSeconds: 60})).Jobs[0]
	if _, err = s.MemoryStage1Commit("commit-output", "empty", claim.LeaseToken, "raw", "summary"); err != nil {
		t.Fatal(err)
	}
	refresh, err = s.MemoryStage1Enqueue("enqueue-empty-again", "empty", "v3", 3000)
	if err != nil || refresh.Status != "committed" {
		t.Fatal(refresh, err)
	}
	claim = committedJob(t, s, jobRequest("claim-empty-again", "memory.job.claim", NativeJobCommand{JobID: "empty", LeaseSeconds: 60})).Jobs[0]
	result, err = s.MemoryStage1Commit("commit-empty-again", "empty", claim.LeaseToken, "", "")
	if err != nil || result.Status != "committed" || len(result.Resources) != 2 || len(result.Jobs) != 2 {
		t.Fatalf("no-output did not atomically delete old pair and queue phase2: %+v %v", result, err)
	}
	for _, resource := range result.Resources {
		if !resource.Deleted {
			t.Fatalf("no-output retained a live stage1 artifact: %+v", resource)
		}
	}
}

func TestNativePhase2LeaseBindsConfiguredInputLimit(t *testing.T) {
	s, _ := openTest(t)
	for _, id := range []string{"a", "b"} {
		committedJob(t, s, jobRequest("enqueue-"+id, "memory.stage1.enqueue", NativeJobCommand{JobID: id, Kind: "stage1", InputVersion: "v1"}))
		claim := committedJob(t, s, jobRequest("claim-"+id, "memory.job.claim", NativeJobCommand{JobID: id, LeaseSeconds: 60})).Jobs[0]
		committedJob(t, s, jobRequest("commit-"+id, "memory.stage1.commit", NativeJobCommand{JobID: id, LeaseToken: claim.LeaseToken},
			NativeChange{Domain: "memory.stage1", Key: "raw/" + id + ".md", Content: []byte("raw " + id)},
			NativeChange{Domain: "memory.stage1", Key: "summary/" + id + ".md", Content: []byte("summary " + id)}))
	}
	begin := committedJob(t, s, jobRequest("begin-limited", "memory.phase2.begin", NativeJobCommand{JobID: "global", LeaseSeconds: 60, MaxInputs: 1})).Jobs[0]
	if begin.SelectionLimit != 1 || len(begin.SelectedRevisions) != 2 {
		t.Fatalf("phase2 lease did not bind one complete stage1 pair: %+v", begin)
	}
	for key := range begin.SelectedRevisions {
		if !strings.HasSuffix(key, "/b.md") {
			t.Fatalf("stable tie-break did not select the newest key: %+v", begin.SelectedRevisions)
		}
	}
}
