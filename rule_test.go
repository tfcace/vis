package vis_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// signal is a value the test controls, read by the rule on each check.
type signal struct{ v atomic.Int64 }

func (s *signal) set(v int64)                { s.v.Store(v) }
func (s *signal) read(*vis.Instance) float64 { return float64(s.v.Load()) }

func batchRule(s *signal) vis.RuleSpec {
	return vis.RuleSpec{
		Name: "batch_backoff", Knob: batchSize,
		Watch: vis.Measure{Name: "sched.p99", Unit: "ms", Read: s.read},
		Over:  vis.Fixed(5 * time.Millisecond),
		For:   3 * time.Second, Step: -16, Cooldown: 10 * time.Second, Lease: time.Hour, Deadband: vis.Fixed(time.Millisecond),
	}
}

func runRule(t *testing.T, in *vis.Instance, r vis.RuleSpec) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go in.RunRule(ctx, r)
}

func stateOf(in *vis.Instance, name string) vis.RuleState {
	for _, r := range in.Rules() {
		if r.Name == name {
			return r
		}
	}
	return vis.RuleState{}
}

func TestARuleActsWhileItHelps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(20)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second) // held over the limit for 3 s: first step
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("after the first step: %d, want 48", got)
		}
		s.set(12) // better by more than the dead band, still over the limit
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 32 {
			t.Fatalf("after a step that helped: %d, want another step to 32", got)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "acting" || st.Stalls != 0 {
			t.Fatalf("state = %+v", st)
		}
	})
}

// The cacheproxy squeeze: the signal never improves, so the rule stops after
// Stall actions instead of walking the knob to its floor.
func TestARuleThatIsNotHelpingStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, a := newAudited()
		s := &signal{}
		s.set(75)
		runRule(t, in, batchRule(s))
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 32 {
			t.Fatalf("batch size = %d, want 32: two steps that didn't help, then stop", got)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "stalled" || st.Stalls != 2 {
			t.Fatalf("state = %+v, want stalled after 2", st)
		}
		var stalled bool
		for _, r := range a.records(t) {
			if r["msg"] == "vis.rule" && r["event"] == "stalled" && r["rule"] == "batch_backoff" {
				stalled = true
			}
		}
		if !stalled {
			t.Fatal("no vis.rule stalled record in the audit trail")
		}
	})
}

func TestAStalledRuleResumesOnceItsConditionClears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(75)
		runRule(t, in, batchRule(s))
		time.Sleep(time.Minute)
		s.set(1) // the condition clears for longer than For
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.State != "watching" {
			t.Fatalf("state = %+v, want watching again", st)
		}
	})
}

func TestARuleMovesAKnobABoundedDistance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(100)
		r := batchRule(s)
		r.MaxDrift = 24 // from the default 64: no lower than 40
		r.Stall = 100   // let it keep trying, to reach the drift bound
		runRule(t, in, r)
		for v := int64(90); v > 50; v -= 10 { // improving each time, never under the limit
			time.Sleep(10 * time.Second)
			s.set(v)
		}
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 40 {
			t.Fatalf("batch size = %d, want 40: 64 minus at most 24", got)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "at_limit" {
			t.Fatalf("state = %+v, want at_limit", st)
		}
	})
}

func TestAShadowRuleOnlyRecordsWhatItWouldDo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, a := newAudited()
		s := &signal{}
		s.set(20)
		r := batchRule(s)
		r.Shadow = true
		runRule(t, in, r)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("a shadow rule changed the knob to %d", got)
		}
		var would bool
		for _, rec := range a.records(t) {
			if rec["msg"] == "vis.rule" && rec["event"] == "would_step" {
				would = true
			}
		}
		if !would {
			t.Fatal("no would_step record")
		}
		// What people and agents read says so too: in Ollama, a shadow rule read
		// "watching" through a breach while the history said it would step.
		if st := stateOf(in, "batch_backoff"); st.State != "shadow" || st.Signal != 20 {
			t.Fatalf("state = %+v, want shadow at signal 20", st)
		}
	})
}

func TestARuleHoldsDuringAFreeze(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustFreeze(t, in, sam, "")
		s := &signal{}
		s.set(75)
		runRule(t, in, batchRule(s))
		time.Sleep(time.Minute)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.State != "frozen" || st.Stalls != 0 {
			t.Fatalf("state = %+v, want frozen, with no stalls counted", st)
		}
	})
}

func TestRulesSayWhatTheyMeasure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, a := newAudited()
		s := &signal{}
		s.set(75)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.SignalName != "sched.p99" || st.Unit != "ms" {
			t.Fatalf("state = %+v, want the signal named, with its unit", st)
		}
		for _, r := range a.records(t) {
			if r["msg"] == "vis.change" {
				if r["reason"] != "sched.p99 75ms > 5ms" {
					t.Fatalf("reason = %q, want the signal, value and limit with units", r["reason"])
				}
				return
			}
		}
		t.Fatal("the rule made no change")
	})
}

// An agent facing a harmful rule can switch it off; its value ends with it.
func TestAnAgentCanDisableARule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(75)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("batch size = %d, want the rule's 48", got)
		}
		if err := in.SetRule("batch_backoff", false, claude, "it lowers batch size while the origin is the bottleneck"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("batch size = %d, want 64: the rule's value ends when it is disabled, and it doesn't act again", got)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "disabled" {
			t.Fatalf("state = %+v", st)
		}
		var disabled bool
		for _, e := range in.History(50) {
			if e.Kind == "rule" && e.Fields["event"] == "disabled" && e.Fields["actor"] == "agent:claude" && e.Fields["reason"] != "" {
				disabled = true
			}
		}
		if !disabled {
			t.Fatal("no audited disable")
		}
		if err := in.SetRule("batch_backoff", true, claude, "origin recovered"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("after re-enabling: %d, want the rule acting again", got)
		}
	})
}

func TestTogglingRulesIsGated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		runRule(t, in, batchRule(s))
		synctest.Wait()
		if err := in.SetRule("batch_backoff", false, sched, ""); !errors.Is(err, vis.ErrNotAllowed) {
			t.Fatalf("a rule toggling a rule: %v, want ErrNotAllowed", err)
		}
		if err := in.SetRule("nope", false, claude, ""); !errors.Is(err, vis.ErrUnknownRule) {
			t.Fatalf("unknown rule: %v", err)
		}
		mustFreeze(t, in, sam, "")
		if err := in.SetRule("batch_backoff", false, claude, ""); !errors.Is(err, vis.ErrFrozen) {
			t.Fatalf("agent toggling during a freeze: %v, want ErrFrozen", err)
		}
		if err := in.SetRule("batch_backoff", false, sam, "looked at it"); err != nil {
			t.Fatalf("a person toggling during a freeze: %v", err)
		}
	})
}

// A rule says where it may take its knob, from the value below it and its
// MaxDrift, when it starts: the floor is the most consequential default.
func TestARuleSaysTheRangeItMayMoveItsKnob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		r := batchRule(s) // Step -16: MaxDrift defaults to 64
		runRule(t, in, r)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.Moves != "0..64" && st.Moves != "1..64" {
			t.Fatalf("moves = %q, want it to say batch_size goes no lower than 1 (64 minus 64, inside its range)", st.Moves)
		}
	})
}

// What a developer writes: knob handles and ready-made measures.
func TestRulesTakeKnobHandlesAndMeasures(t *testing.T) {
	r := vis.RuleSpec{Name: "typed", Knob: batchSize, Watch: vis.SchedP99, Over: timeout, Step: -1, Lease: time.Minute}
	if r.Watch.Name != "sched.p99" || r.Watch.Unit != "ms" {
		t.Fatalf("SchedP99 = %+v", r.Watch)
	}
	in := vis.NewInstance()
	hits := in.Signal("hits", "", "", func() float64 { return 0.5 })
	if hits.Name != "hits" || hits.Read(in) != 0.5 {
		t.Fatalf("an app signal as a measure: %+v", hits)
	}
	if v := vis.RouteP99("GET /nothing").Read(in); v != 0 {
		t.Fatalf("p99 of an idle route = %v", v)
	}
}

// The reply to switching a rule off says it's off, not what it was a moment ago.
func TestSetRuleReportsTheNewStateAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		runRule(t, in, batchRule(s))
		synctest.Wait()
		if err := in.SetRule("batch_backoff", false, claude, "testing"); err != nil {
			t.Fatal(err)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "disabled" { // no tick has passed
			t.Fatalf("right after disabling: %+v", st)
		}
		if err := in.SetRule("batch_backoff", true, claude, "testing"); err != nil {
			t.Fatal(err)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "watching" {
			t.Fatalf("right after enabling: %+v", st)
		}
	})
}

var cpuSpender = vis.NewInt("test_cpu_spender", vis.Range(1, 9), vis.Default(9), vis.Uses(vis.CPU), vis.Pull(vis.Rule))

// A rule that watches CPU but moves a knob that doesn't spend CPU is suspect; vis says so.
func TestARuleWhoseKnobCannotHelpCarriesAWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		zero := vis.Fixed(time.Hour)
		runRule(t, in, vis.RuleSpec{Name: "mismatched", Knob: batchSize, Watch: vis.SchedP99, Over: zero, Step: -1, Lease: time.Minute})
		runRule(t, in, vis.RuleSpec{Name: "matched", Knob: cpuSpender, Watch: vis.SchedP99, Over: zero, Step: -1, Lease: time.Minute})
		synctest.Wait()
		if w := stateOf(in, "mismatched").Warning; !strings.Contains(w, "test_batch_size doesn't spend cpu, which sched.p99 measures") {
			t.Fatalf("warning = %q", w)
		}
		if w := stateOf(in, "mismatched").Warning; !strings.Contains(w, "declare vis.Uses(vis.CPU) on the knob") {
			t.Fatalf("the warning's fix must name an identifier that exists: %q", w)
		}
		if w := stateOf(in, "matched").Warning; w != "" {
			t.Fatalf("a rule whose knob spends what it watches got a warning: %q", w)
		}
	})
}

// A switched-off rule still says what it would act on: its limit is the
// rule's, not a zero that reads as a threshold of nothing.
func TestASwitchedOffRuleReportsItsLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(1)
		runRule(t, in, batchRule(s))
		synctest.Wait()
		if err := in.SetRule("batch_backoff", false, claude, "not now"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.State != "disabled" || st.Limit != 5 {
			t.Fatalf("state = %q, limit = %v, want disabled with its 5 ms limit", st.State, st.Limit)
		}
	})
}

// originSpike is cacheproxy's origin-spike in miniature: while the batch size
// is at its default the scheduler lags, and any smaller batch quiets the
// scheduler but makes every request slow. A rule watching the scheduler sees
// its step help; the SLO says it made things worse.
func originSpike(t *testing.T, in *vis.Instance) (rule vis.RuleSpec, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) {
		d := 20 * time.Millisecond
		if batchSize.From(r.Context()) < 64 {
			d = 300 * time.Millisecond
		}
		time.Sleep(d)
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
	rule.Watch.Read = func(in *vis.Instance) float64 {
		if batchSize.Get(in.Snapshot()) < 64 {
			return 1
		}
		return 20
	}
	return rule, func() { close(done); <-exited }
}

// A rule acts on its signal and answers to the SLO: when its step quiets the
// signal but the SLO gets worse, vis undoes the rule's change and stops it
// until a person or an agent switches it back on.
func TestARuleThatWorsensTheSLOIsUndoneAndStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		r, stop := originSpike(t, in)
		defer stop()
		time.Sleep(20 * time.Second) // a baseline at 20 ms
		runRule(t, in, r)
		time.Sleep(4 * time.Second) // over its limit for 3 s: the first step
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("after the first step: %d, want 48", got)
		}
		time.Sleep(11 * time.Second) // the cooldown: the step is judged
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("after a step that hurt the SLO: %d, want the rule's change undone (64)", got)
		}
		st := stateOf(in, "batch_backoff")
		if st.State != "stopped" {
			t.Fatalf("state = %+v, want stopped", st)
		}
		has(t, st.Stopped, "sched.p99 improved")
		has(t, st.Stopped, "GET /k p99 got worse")

		time.Sleep(time.Minute) // its signal is over the limit again, but it stays stopped
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("a stopped rule acted again: %d", got)
		}
		story := only(t, in.Stories(10, sam), 1)[0]
		if !story.Alert || !strings.Contains(story.Title, "then vis undid it and stopped the rule") {
			t.Fatalf("story = %+v", story)
		}
		has(t, story.Body, "test_batch_size is back to 64")
		if len(story.Effects) != 2 || story.Effects[0].Name != "sched.p99" || story.Effects[0].Worse ||
			story.Effects[1].Name != "GET /k p99" || !story.Effects[1].Worse || story.Effects[1].To <= story.Effects[1].From {
			t.Fatalf("effects = %+v, want the signal that improved and the SLO that got worse", story.Effects)
		}
		if story.Why != "Its lever is the wrong one for what is happening now." {
			t.Fatalf("why = %q", story.Why)
		}

		if err := in.SetRule("batch_backoff", true, sam, "the origin is fixed"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("switched back on: %d, want it acting again (48)", got)
		}
	})
}

// A step that helps the signal and doesn't hurt the SLO is a rule doing its job.
func TestARuleWhoseStepKeepsTheSLOKeepsGoing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		defer slowService(t, in)() // its latency doesn't depend on the batch size
		if _, err := in.Apply(vis.Tx{By: sam, Reason: "a fast baseline", Set: []vis.Write{{Knob: "test_slowness", Value: 10}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Second) // past that change's measurement
		s := &signal{}
		s.set(20)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second)
		synctest.Wait()
		s.set(12)
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.State != "acting" || batchSize.Get(in.Snapshot()) != 32 {
			t.Fatalf("state = %+v, batch = %d; want it acting, at 32", st, batchSize.Get(in.Snapshot()))
		}
	})
}

// While a person's or an agent's change is being measured, rules hold off, so
// its measurement isn't confounded by their steps.
func TestRulesHoldWhileAChangeIsMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		defer slowService(t, in)()
		time.Sleep(20 * time.Second)
		s := &signal{}
		s.set(20)
		if _, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "try it", Set: []vis.Write{{Knob: "test_slowness", Value: 10}}}); err != nil {
			t.Fatal(err)
		}
		runRule(t, in, batchRule(s))
		time.Sleep(10 * time.Second) // over its limit for long enough, but claude's change is still being measured
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("a rule stepped during a measurement: %d", got)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "holding" {
			t.Fatalf("state = %+v, want holding", st)
		}
		time.Sleep(21 * time.Second) // the measurement is done
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("after the measurement: %d, want the rule's first step (48)", got)
		}
		if im, _ := in.Impact(1); len(im.Confounded) != 0 {
			t.Fatalf("claude's change was confounded: %+v", im)
		}
	})
}

// do=set: the rule puts its knob at one value, and holds it there.
func TestARuleCanSetItsKnob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(20)
		r := batchRule(s)
		r.Step, r.Do = 0, vis.SetTo(8)
		runRule(t, in, r)
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 8 {
			t.Fatalf("after its action: %d, want 8", got)
		}
		if st := stateOf(in, "batch_backoff"); st.Moves != "8" {
			t.Fatalf("moves = %q, want 8", st.Moves)
		}
		s.set(12)                    // it helped, but the signal is still over its limit
		time.Sleep(11 * time.Second) // already at 8: nothing more to do
		synctest.Wait()
		if got, st := batchSize.Get(in.Snapshot()), stateOf(in, "batch_backoff"); got != 8 || st.State != "at_limit" {
			t.Fatalf("batch = %d, state = %+v; want it holding 8", got, st)
		}
	})
}

// do=reset: one rule undoes what rules set on a knob, so it falls back to the
// value below them: "when the origin backs up, stop shrinking batches".
func TestARuleCanResetWhatRulesSet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		sched, origin := &signal{}, &signal{}
		sched.set(20)
		runRule(t, in, batchRule(sched))
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("after the first rule: %d, want 48", got)
		}
		origin.set(3000)
		runRule(t, in, vis.RuleSpec{Name: "batch_restore", Knob: batchSize, Do: vis.Reset(),
			Watch: vis.Measure{Name: "origin.wait", Unit: "ms", Read: origin.read}, Over: vis.Fixed(2 * time.Second),
			For: time.Second, Cooldown: 10 * time.Second, Lease: time.Minute})
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("after the reset: %d, want 64, the value below the rules", got)
		}
	})
}

// A rule watching a route's p99 judges its step by the requests served after
// it, not by a minute that is mostly from before.
func TestARuleWatchingARouteJudgesTheRequestsAfterItsStep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
			if batchSize.From(r.Context()) >= 64 {
				time.Sleep(300 * time.Millisecond)
			} else {
				time.Sleep(20 * time.Millisecond)
			}
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
					serve(h, "GET", "/slow")
					time.Sleep(50 * time.Millisecond)
				}
			}
		}()
		defer func() { close(done); <-exited }()
		time.Sleep(30 * time.Second) // a slow minute's start
		runRule(t, in, vis.RuleSpec{Name: "route_backoff", Knob: batchSize, Watch: vis.RouteP99("GET /slow"),
			Over: vis.Fixed(100 * time.Millisecond), For: 3 * time.Second, Step: -16, Cooldown: 15 * time.Second, Lease: time.Hour})
		time.Sleep(45 * time.Second) // the step, then two checks after its cooldown
		synctest.Wait()
		if got, st := batchSize.Get(in.Snapshot()), stateOf(in, "route_backoff"); got != 48 || st.State == "stalled" {
			t.Fatalf("batch = %d, state = %+v; want the one step that helped, kept", got, st)
		}
	})
}

// A signal that panics leaves its rule in an error state; the rule, and the process, carry on.
func TestARuleWhoseSignalPanicsReportsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		r := batchRule(nil)
		r.Watch.Read = func(*vis.Instance) float64 { panic("boom") }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go in.RunRule(ctx, r)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		st := stateOf(in, "batch_backoff")
		if st.State != "error" || !strings.Contains(st.Err, "boom") {
			t.Fatalf("state = %+v, want an error naming the panic", st)
		}
	})
}

// A rule whose knob doesn't let rules write it stays in error, saying why,
// rather than looking healthy between attempts.
func TestARuleItsKnobDoesNotPullStaysInError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(20)
		r := batchRule(s)
		r.Knob = slowness                                                 // pulls agents only
		r.For, r.Every, r.Cooldown = 0, 100*time.Millisecond, time.Second // as an adopter's probe had it
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go in.RunRule(ctx, r)
		time.Sleep(4 * time.Second) // its first attempt fails
		synctest.Wait()
		for range 40 { // later checks don't hide it, even between them
			time.Sleep(25 * time.Millisecond)
			if st := stateOf(in, "batch_backoff"); st.State != "error" || !strings.Contains(st.Err, "rule") {
				t.Fatalf("state = %+v, want error saying rules may not write the knob", st)
			}
		}
		if st := stateOf(in, "batch_backoff"); !strings.Contains(st.Warning, "vis.Pull(vis.Rule)") {
			t.Fatalf("warning = %q, want it to say how to fix the declaration", st.Warning)
		}
		errs := 0
		for _, e := range in.History(vis.MaxHistory) {
			if e.Kind == "rule" && e.Fields["event"] == "error" {
				errs++
			}
		}
		if errs != 1 {
			t.Fatalf("%d error events, want one: the same error isn't news on every check", errs)
		}
	})
}

// A rule whose read fails, then works again, goes back to watching.
func TestARuleRecoversWhenItsReadWorksAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		var fails atomic.Int32
		fails.Store(2)
		r := batchRule(nil)
		r.Watch.Read = func(*vis.Instance) float64 {
			if fails.Add(-1) >= 0 {
				panic("not ready")
			}
			return 1 // under its limit
		}
		r.For = time.Hour // so only the read working again can clear the error, not the condition clearing
		runRule(t, in, r)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.State != "watching" || st.Err != "" {
			t.Fatalf("state = %+v, want watching again, without the old error", st)
		}
	})
}

// A rule says what it does, and where it may take its knob after the value
// below it changes.
func TestARuleSaysItsActionAndCurrentReach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(20)
		if _, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "smaller", Set: []vis.Write{{Knob: batchSize.Name(), Value: 32}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second) // past its measurement
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.Action != "step -16" || st.Moves != "1..32" {
			t.Fatalf("action %q, moves %q; want step -16 within 1..32, from the agent's 32", st.Action, st.Moves)
		}
	})
}

// A percent step moves the knob by a share of its value, so it never lurches
// near the bottom of its range, and reaches four such steps by default.
func TestAPercentStepScalesWithTheKnob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(90)
		r := batchRule(s)
		r.Step, r.Do = 0, vis.StepPercent(-25)
		runRule(t, in, r)
		time.Sleep(time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.Action != "step -25%" || st.Moves != "20..64" {
			t.Fatalf("action %q, moves %q; want step -25%% within 20..64: four quarters off 64", st.Action, st.Moves)
		}
		var got []int
		for v := int64(80); v > 30; v -= 10 { // improving each time, never under the limit
			time.Sleep(10 * time.Second)
			synctest.Wait()
			got = append(got, batchSize.Get(in.Snapshot()))
			s.set(v)
		}
		if want := []int{48, 36, 27, 20, 20}; !slices.Equal(got, want) {
			t.Fatalf("batch size after each step: %v, want %v", got, want)
		}
		if st := stateOf(in, "batch_backoff"); st.State != "at_limit" {
			t.Fatalf("state = %+v, want at_limit", st)
		}
	})
}

// A percent step on a small integer still moves it by one.
func TestAPercentStepMovesASmallKnobByOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		if _, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "small", Set: []vis.Write{{Knob: batchSize.Name(), Value: 2}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second) // past its measurement
		s := &signal{}
		s.set(20)
		r := batchRule(s)
		r.Step, r.Do = 0, vis.StepPercent(-25)
		runRule(t, in, r)
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 1 {
			t.Fatalf("batch size = %d, want 1: a quarter of 2 rounds to nothing, so one", got)
		}
	})
}

// A person's change while a rule's step is being judged doesn't get the rule
// blamed for what the person did.
func TestAPersonsChangeIsNotBlamedOnARule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		defer slowService(t, in)()
		if _, err := in.Apply(vis.Tx{By: sam, Reason: "fast", Set: []vis.Write{{Knob: "test_slowness", Value: 10}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Second)
		s := &signal{}
		s.set(20)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second) // the rule steps
		synctest.Wait()
		s.set(1) // its signal improves
		if _, err := in.Apply(vis.Tx{By: sam, Reason: "testing slow", Set: []vis.Write{{Knob: "test_slowness", Value: 200}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(11 * time.Second) // the step is judged: p99 got much worse, but by sam's change
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.State == "stopped" {
			t.Fatalf("the rule was stopped for a person's change: %+v", st)
		}
	})
}

// A rule whose steps quiet its signal but leave the SLO broken, no closer to
// its target, is undone and stopped after Stall such steps, as one that makes
// it worse is: its lever doesn't reach what's wrong.
func TestARuleThatLeavesTheSLOBrokenIsUndoneAndStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		defer slowService(t, in)() // 200 ms whatever the batch size
		time.Sleep(20 * time.Second)
		s := &signal{}
		s.set(20)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second) // the first step
		synctest.Wait()
		s.set(15)                    // its signal improves, still over its limit
		time.Sleep(10 * time.Second) // judged: no closer to 50 ms; a second step
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 32 {
			t.Fatalf("after one step that didn't help the SLO: %d, want a second step (32)", got)
		}
		s.set(10)
		time.Sleep(11 * time.Second) // judged again: two in a row
		synctest.Wait()
		st := stateOf(in, "batch_backoff")
		if st.State != "stopped" || batchSize.Get(in.Snapshot()) != 64 {
			t.Fatalf("state = %+v, batch = %d; want stopped, with its change undone (64)", st, batchSize.Get(in.Snapshot()))
		}
		has(t, st.Stopped, "GET /k p99 stayed over its 50 ms objective")
		story := only(t, in.Stories(10, sam), 1)[0]
		has(t, story.Title, "its steps didn't bring the SLO closer")
	})
}

// Each step is judged from where the step before it left the SLO, so a step
// isn't credited with what the one before it did. Here every step closes a
// fifth of the gap: none is enough, and two in a row stop the rule. Judged
// against the SLO over the window before the step, which still holds requests
// from before the previous step, the second looks like it closed a third.
func TestAStepIsJudgedFromWhereTheStepBeforeLeftTheSLO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		latency := map[int]int{64: 250, 48: 210, 32: 178, 16: 152} // the gap to 50 ms: 200, 160, 128, 102
		defer serving(in, func(r *http.Request) {
			time.Sleep(time.Duration(latency[batchSize.From(r.Context())]) * time.Millisecond)
		})()
		time.Sleep(20 * time.Second)
		s := &signal{}
		s.set(20)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second) // the first step
		synctest.Wait()
		s.set(15)
		time.Sleep(10 * time.Second) // judged: 250 → 210 ms, not enough; the second step
		synctest.Wait()
		s.set(10)
		time.Sleep(11 * time.Second) // judged: 210 → 178 ms, not enough, two in a row
		synctest.Wait()
		st := stateOf(in, "batch_backoff")
		if st.State != "stopped" || batchSize.Get(in.Snapshot()) != 64 {
			t.Fatalf("state = %+v, batch = %d; want stopped after two steps that each closed a fifth of the gap, undone (64)", st, batchSize.Get(in.Snapshot()))
		}
		has(t, st.Stopped, "no closer to it (210 ms → ") // from where the first step left it, not 250
	})
}

// serving sends GET /k to in every 50 ms until stopped, each answered by h.
func serving(in *vis.Instance, h func(*http.Request)) (stop func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) { h(r) })
	handler := in.HTTP(mux)
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case <-done:
				return
			default:
				serve(handler, "GET", "/k")
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	return func() { close(done); <-exited }
}

// A step that brings the SLO a good part of the way back counts as helping,
// even if the SLO is still broken: the rule goes on.
func TestARuleThatBringsTheSLOCloserGoesOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		defer serving(in, func(r *http.Request) {
			time.Sleep(time.Duration(30+batchSize.From(r.Context())*3) * time.Millisecond) // 222 ms at 64, 174 at 48, 126 at 32
		})()
		time.Sleep(20 * time.Second)
		s := &signal{}
		s.set(20)
		runRule(t, in, batchRule(s))
		time.Sleep(4 * time.Second)
		synctest.Wait()
		s.set(15)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		s.set(10)
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.State == "stopped" {
			t.Fatalf("a rule bringing the SLO closer was stopped: %+v", st)
		}
	})
}
