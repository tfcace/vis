package vis_test

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// A request a service refuses (429) is no more served than one that failed
// (5xx), and it is fast, so it pulls p99 down: vis counts it apart, so a
// change that sheds load can't read as only making requests faster.
func TestARefusedRequestIsCountedApart(t *testing.T) {
	in := vis.NewInstance()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("s") {
		case "429":
			w.WriteHeader(http.StatusTooManyRequests)
		case "503":
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	h := in.HTTP(mux)
	for _, s := range []string{"", "", "429", "503"} {
		serve(h, "GET", "/k?s="+s)
	}
	r := in.Route("GET /k", time.Minute)
	if r.Requests != 4 || r.Refused != 1 || r.Errors != 1 {
		t.Fatalf("route = %+v, want 4 requests, 1 refused (the 429) and 1 error (the 503)", r)
	}
}

// failFast is imageproxy's slow origin in miniature: at the default batch size
// requests take 100 ms, over the 50 ms objective; any smaller batch fails them
// at once with a 504. A rule watching p99 sees its step help: p99 falls,
// because every request now fails fast.
func failFast(t *testing.T, in *vis.Instance) (rule vis.RuleSpec, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) {
		if batchSize.From(r.Context()) < 64 {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		time.Sleep(100 * time.Millisecond)
	})
	h := in.HTTP(mux)
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case <-done:
				return
			default:
				serve(h, "GET", "/k")
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	rule = batchRule(nil)
	rule.Watch = vis.RouteP99("GET /k")
	rule.Over = vis.Fixed(50 * time.Millisecond)
	return rule, func() { close(done); <-exited }
}

// A step that makes the objective's requests fail is harm, however fast they
// fail: vis undoes it and stops the rule, as a rollout's gate does. In
// the live-rules eval an imageproxy rule held its time limit at 1s with every
// image failing, because its p99 kept improving.
func TestARuleThatMakesRequestsFailIsUndoneAndStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		r, stop := failFast(t, in)
		defer stop()
		time.Sleep(20 * time.Second) // a baseline at 100 ms, served
		runRule(t, in, r)
		time.Sleep(4 * time.Second) // over its limit for 3 s: the first step
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("after the first step: %d, want 48", got)
		}
		time.Sleep(11 * time.Second) // the cooldown: the step is judged
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("after a step that failed every request: %d, want the rule's change undone (64)", got)
		}
		st := stateOf(in, "batch_backoff")
		if st.State != "stopped" {
			t.Fatalf("state = %+v, want stopped", st)
		}
		has(t, st.Stopped, "GET /k failed 0% → 100%")
		story := only(t, in.Stories(10, sam), 1)[0]
		if !story.Alert || !strings.Contains(story.Title, "then vis undid it and stopped the rule: requests failed") {
			t.Fatalf("story = %+v", story)
		}
	})
}

// An impact report says how much of a route went unserved before and after,
// beside its p99: "p99 fell" from failing or refusing requests is no win.
func TestAnImpactReportSaysWhatWentUnserved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		_, stop := failFast(t, in)
		defer stop()
		time.Sleep(30 * time.Second)
		rec, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "faster", Set: []vis.Write{{Knob: "test_batch_size", Value: 32}}})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second)
		synctest.Wait()
		im, _ := in.Impact(rec.Change)
		if !im.Done || !strings.Contains(im.Summary, "GET /k failed 0% → 100%") {
			t.Fatalf("summary = %q, want the failed share", im.Summary)
		}
	})
}
