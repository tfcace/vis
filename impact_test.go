package vis_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

var slowness = vis.NewInt("test_slowness", vis.Range(1, 200), vis.Default(200), vis.Unit("ms"), vis.Uses(vis.CPU), vis.Pull(vis.Agent))

// a service whose latency is the slowness knob, under steady traffic
func slowService(t *testing.T, in *vis.Instance) (stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(slowness.From(r.Context())) * time.Millisecond)
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
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	return func() { close(done); <-exited }
}

// vis measures what a change did, with no declaration beyond the SLO.
func TestEveryChangeGetsABeforeAndAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		defer slowService(t, in)()
		time.Sleep(30 * time.Second) // a slow baseline
		rec, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "too slow", Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		if err != nil {
			t.Fatal(err)
		}
		if im, ok := in.Impact(rec.Change); !ok || im.Done {
			t.Fatalf("right after the change: %+v, %v; want a pending report", im, ok)
		}
		time.Sleep(31 * time.Second)
		synctest.Wait()
		im, _ := in.Impact(rec.Change)
		if !im.Done || len(im.Confounded) != 0 {
			t.Fatalf("after settling: %+v", im)
		}
		// The test process's own live heap may move too, as any process's does: a report may say so after the p99.
		if !regexp.MustCompile(`^After 30s: GET /k p99 (19\d|20\d) → 10 ms(, live heap growth .+ MiB/s)?\.$`).MatchString(im.Summary) {
			t.Fatalf("summary = %q", im.Summary)
		}
		st := in.Stories(10, sam)[0]
		if !strings.Contains(st.Body, im.Summary) {
			t.Fatalf("story = %q, want the measured effect in it", st.Body)
		}
		// the same, as fields a page draws without reading sentences
		effects := slices.DeleteFunc(slices.Clone(st.Effects), func(e vis.Moved) bool { return e.Name == "live heap growth" })
		if st.Why != "too slow" || st.After != "30s" || len(effects) != 1 {
			t.Fatalf("why %q, after %q, effects %+v", st.Why, st.After, st.Effects)
		}
		if e := effects[0]; e.Name != "GET /k p99" || e.Unit != "ms" || e.To != 10 || e.From < 190 || e.From >= 210 || e.Worse {
			t.Fatalf("effect = %+v", e)
		}
	})
}

func TestOverlappingChangesAreConfounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		defer slowService(t, in)()
		time.Sleep(30 * time.Second)
		rec, _ := in.Apply(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_slowness", Value: 100}}})
		time.Sleep(10 * time.Second)
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: sam})
		time.Sleep(25 * time.Second)
		synctest.Wait()
		im, _ := in.Impact(rec.Change)
		if !im.Done || len(im.Confounded) == 0 || !strings.Contains(im.Confounded[0], "test_cache_size") {
			t.Fatalf("impact = %+v, want it confounded by the cache change", im)
		}
		if !strings.Contains(im.Summary, "confounded") {
			t.Fatalf("summary = %q, want it to say so", im.Summary)
		}
		for _, st := range in.Stories(10, sam) {
			if st.Knob == "test_slowness" && (len(st.Confounded) == 0 || !strings.Contains(st.Confounded[0], "test_cache_size")) {
				t.Fatalf("story confounded = %q", st.Confounded)
			}
		}
	})
}

// Levers with a measured effect come first, with the evidence; position only breaks ties.
func TestLeversRankByWhatTheyDidBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		defer slowService(t, in)()
		time.Sleep(30 * time.Second)
		mustSet(t, in, vis.Change{Knob: "test_slowness", Value: 10, For: time.Hour, By: claude})
		time.Sleep(35 * time.Second)
		mustSet(t, in, vis.Change{Knob: "test_slowness", Value: 150, For: time.Hour, By: claude}) // back up, so its position is no longer the top
		time.Sleep(35 * time.Second)
		synctest.Wait()
		ls := in.Levers(vis.CPU)
		var mine vis.Lever
		for _, l := range ls {
			if l.Knob == "test_slowness" {
				mine = l
			}
		}
		if !strings.Contains(mine.Observed, "p99") || ls[0].Knob != "test_slowness" {
			t.Fatalf("levers = %s; want test_slowness first, with what it did", fmt.Sprint(ls))
		}
	})
}

// In an incident something else is usually changing too; that evidence still counts, labeled.
func TestConfoundedEvidenceStillNamesALever(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		defer slowService(t, in)()
		time.Sleep(30 * time.Second)
		mustSet(t, in, vis.Change{Knob: "test_slowness", Value: 10, For: time.Hour, By: claude})
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: sam})
		time.Sleep(35 * time.Second)
		synctest.Wait()
		for _, l := range in.Levers(vis.CPU) {
			if l.Knob == "test_slowness" {
				if !strings.Contains(l.Observed, "while other things changed too") {
					t.Fatalf("observed = %q", l.Observed)
				}
				return
			}
		}
		t.Fatal("test_slowness is not a lever")
	})
}

// Waiting for a change's measurement returns it the moment it is done.
func TestWaitImpactReturnsOnceMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		defer slowService(t, in)()
		time.Sleep(30 * time.Second)
		rec, _ := in.Apply(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		start := time.Now()
		im, err := in.WaitImpact(context.Background(), rec.Change)
		if err != nil || !im.Done || !strings.Contains(im.Summary, "→ 10 ms") {
			t.Fatalf("WaitImpact = %+v, %v", im, err)
		}
		if waited := time.Since(start); waited < 30*time.Second || waited > 31*time.Second {
			t.Fatalf("waited %v, want the settle time", waited)
		}
		if again, err := in.WaitImpact(context.Background(), rec.Change); err != nil || again.Summary != im.Summary {
			t.Fatalf("waiting on a measured change = %+v, %v; want it at once", again, err)
		}
	})
}

func TestWaitImpactStopsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		rec, _ := in.Apply(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		im, err := in.WaitImpact(ctx, rec.Change)
		if !errors.Is(err, context.DeadlineExceeded) || im.Done || im.Change != rec.Change {
			t.Fatalf("WaitImpact = %+v, %v; want the pending report and the deadline", im, err)
		}
		if _, err := in.WaitImpact(context.Background(), 99999); err == nil {
			t.Fatal("waited on an unknown change")
		}
		time.Sleep(31 * time.Second) // let the pending measurement finish inside the bubble
	})
}

// A report dropped from the table while someone waits on it still reaches the waiter.
func TestWaitImpactSurvivesEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance())
		rec, _ := in.Apply(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		got := make(chan vis.Impact, 1)
		go func() { im, _ := in.WaitImpact(context.Background(), rec.Change); got <- im }()
		synctest.Wait()      // the waiter is waiting before the change is evicted
		for i := range 201 { // more changes than the table keeps
			in.Apply(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_slowness", Value: int64(20 + i%100)}}})
		}
		im := <-got
		if im.Change != rec.Change || !im.Done || im.Summary == "" {
			t.Fatalf("WaitImpact after eviction = %+v, want the measured report", im)
		}
	})
}

// Without an SLO, a change is measured by the routes that carry the traffic:
// a service gains impact reports before it declares objectives.
func TestAChangeWithoutAnSLOIsMeasuredByTheBusiestRoutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance())
		defer slowService(t, in)()
		time.Sleep(30 * time.Second)
		rec, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "too slow", Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second)
		synctest.Wait()
		im, _ := in.Impact(rec.Change)
		if !regexp.MustCompile(`^After 30s: GET /k p99 (19\d|20\d) → 10 ms(, live heap growth .+ MiB/s)?\.$`).MatchString(im.Summary) { // the process's own heap may move too
			t.Fatalf("summary = %q, want GET /k's p99 measured with no SLO declared", im.Summary)
		}
	})
}

// A change's first seconds aren't its effect: a backlog draining right after
// it is slow whatever the change did. The after window takes requests from
// buckets that began well after the change. In fleet-squeeze, a canary that
// had recovered within seconds read "p99 1141 → 1141 ms".
func TestTheAfterWindowLeavesOutTheChangesFirstSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.Settled(vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond)))
		var changedAt time.Time
		var mu sync.Mutex
		mux := http.NewServeMux()
		mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			draining := !changedAt.IsZero() && time.Since(changedAt) < 9*time.Second
			mu.Unlock()
			d := time.Duration(slowness.From(r.Context())) * time.Millisecond
			if draining {
				d = 1500 * time.Millisecond // the backlog from before the change
			}
			time.Sleep(d)
		})
		h := in.HTTP(mux)
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
					serve(h, "GET", "/k")
					time.Sleep(100 * time.Millisecond)
				}
			}
		})
		defer func() { close(done); wg.Wait() }()
		time.Sleep(30 * time.Second)
		mu.Lock()
		changedAt = time.Now()
		mu.Unlock()
		rec, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "too slow", Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		if err != nil {
			t.Fatal(err)
		}
		im, err := in.WaitImpact(context.Background(), rec.Change)
		if err != nil {
			t.Fatal(err)
		}
		if p := im.After["GET /k p99"]; p > 15 {
			t.Fatalf("after p99 = %v ms; want the change's 10 ms, not the drain's 1.5 s: %s", p, im.Summary)
		}
	})
}
