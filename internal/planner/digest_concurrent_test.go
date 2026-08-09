package planner_test

import (
	"sync"
	"testing"

	"github.com/lajosdeme/mole/internal/planner"
)

// M5 slice 3.
//
// The Digest is the one piece of executor state a worker pool genuinely shares.
// Everything else was audited and is already safe: WebActor assigns to no
// receiver field, the rate limiter and the robots cache carry mutexes,
// pricing.Table has an RWMutex, cache and estimator are synchronised, and
// Planner and Verifier hold configuration only. This did not.
//
// The counters are asserted as well as the race detector being run, because
// -race did NOT catch M7's shared-actor mutation. A lost update shows up here as
// a wrong total whether or not the detector notices the write.

func TestDigestSurvivesConcurrentRecording(t *testing.T) {
	d := planner.NewDigest("the research question", 0)
	qs := d.AddQuestions([]planner.SubQuestion{{Text: "a"}, {Text: "b"}, {Text: "c"}})
	if len(qs) != 3 {
		t.Fatalf("fixture registered %d questions, want 3", len(qs))
	}

	const (
		workers   = 8
		perWorker = 50
	)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, so the writes genuinely interleave
			for i := 0; i < perWorker; i++ {
				q := qs[i%len(qs)]
				d.RecordLead(q.ID)
				d.RecordClaims(q.ID, 2)
				d.RecordDeadEnd("bot_block", "a blocked query")
				_ = d.Open()
			}
		}()
	}
	close(start)
	wg.Wait()

	const total = workers * perWorker

	if d.LeadsRun != total {
		t.Errorf("LeadsRun = %d, want %d — %d updates were lost", d.LeadsRun, total, total-d.LeadsRun)
	}
	if d.ClaimsFound != total*2 {
		t.Errorf("ClaimsFound = %d, want %d", d.ClaimsFound, total*2)
	}

	// One cause, collapsed, with every occurrence counted. A torn append shows
	// up as a short slice; a lost increment as a short count.
	if len(d.DeadEnds) != 1 {
		t.Fatalf("%d dead-end causes, want 1 collapsed entry", len(d.DeadEnds))
	}
	if d.DeadEnds[0].Count != total {
		t.Errorf("dead-end count = %d, want %d", d.DeadEnds[0].Count, total)
	}

	// Per-question totals have to add up to the session totals, or coverage is
	// being credited to the wrong thread of the research.
	var leads, claims int
	for _, q := range d.Open() {
		leads += q.Leads
		claims += q.Claims
	}
	if leads != total || claims != total*2 {
		t.Errorf("per-question totals are leads=%d claims=%d, want %d and %d",
			leads, claims, total, total*2)
	}
}
