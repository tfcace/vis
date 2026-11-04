package vis_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// A rule's action is checked when it is declared: one action, its value
// inside the knob's range and of the knob's type.
func TestARulesActionIsCheckedWhenDeclared(t *testing.T) {
	ok := vis.RuleSpec{Knob: batchSize, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Cooldown: time.Second, Lease: time.Minute}
	cases := []struct {
		why  string
		edit func(*vis.RuleSpec)
		want string
	}{
		{"two actions", func(r *vis.RuleSpec) { r.Step, r.Do = -16, vis.SetTo(8) }, "one action"},
		{"no action", func(r *vis.RuleSpec) {}, "Step"},
		{"a value outside the range", func(r *vis.RuleSpec) { r.Do = vis.SetTo(1000) }, "1..512"},
		{"a step wider than the range", func(r *vis.RuleSpec) { r.Step = -600 }, "1..512"},
		{"a duration for an Int knob", func(r *vis.RuleSpec) { r.Do = vis.SetTo(time.Second) }, "integer"},
		{"a percent step of nothing", func(r *vis.RuleSpec) { r.Do = vis.StepPercent(0) }, "never moves"},
		{"a cut of all of it", func(r *vis.RuleSpec) { r.Do = vis.StepPercent(-100) }, "more than -100%"},
	}
	for _, c := range cases {
		r := ok
		r.Name = uniq("test_action")
		c.edit(&r)
		if p := panicOf(func() { vis.NewRule(r) }); !strings.Contains(p, c.want) {
			t.Errorf("%s: panic = %q, want it to mention %q", c.why, p, c.want)
		}
	}
}

// A declared rule is checked when it is declared, and says how to fix it.
func TestADeclaredRuleIsCheckedWhenDeclared(t *testing.T) {
	ok := vis.RuleSpec{Knob: batchSize, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Step: -16, Cooldown: 10 * time.Second, Lease: time.Minute}
	cases := []struct {
		why  string
		edit func(*vis.RuleSpec)
		want string
	}{
		{"its knob doesn't let rules write it", func(r *vis.RuleSpec) { r.Knob = slowness }, "vis.Pull(vis.Rule)"},
		{"a number against a latency", func(r *vis.RuleSpec) { r.Over = vis.Fixed(0.5) }, "time.Duration"},
		{"a latency dead band against a number", func(r *vis.RuleSpec) {
			r.Watch, r.Over, r.Deadband = vis.NewGauge(uniq("test_ratio")).Measure(), vis.Fixed(0.9), vis.Fixed(time.Millisecond)
		}, "dead band"},
		{"no step", func(r *vis.RuleSpec) { r.Step = 0 }, "Step"},
		{"no lease", func(r *vis.RuleSpec) { r.Lease = 0 }, "Lease"},
		{"no threshold", func(r *vis.RuleSpec) { r.Over = nil }, "Over"},
	}
	for _, c := range cases {
		r := ok
		r.Name = uniq("test_bad_rule")
		c.edit(&r)
		if p := panicOf(func() { vis.NewRule(r) }); !strings.Contains(p, c.want) {
			t.Errorf("%s: panic = %q, want it to mention %q", c.why, p, c.want)
		}
	}
	r := ok
	r.Name = uniq("test_good_rule")
	r.Knob = vis.NewInt(uniq("test_good_knob"), vis.Range(1, 512), vis.Default(64), vis.Pull(vis.Rule)) // declared rules run on the default instance: keep them off shared knobs
	if p := panicOf(func() { vis.NewRule(r) }); p != "" {
		t.Fatalf("a good rule panicked: %s", p)
	}
}

// A threshold can be a knob, so people can tune it; a Duration knob is a latency.
func TestARuleThresholdCanBeAKnob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(1500) // ms: under timeout's default of 2 s
		r := batchRule(s)
		r.Over = timeout // 2s
		runRule(t, in, r)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if st := stateOf(in, "batch_backoff"); st.Limit != 2000 || batchSize.Get(in.Snapshot()) != 64 {
			t.Fatalf("state = %+v, want a 2000 ms limit and no step", st)
		}
	})
}

// RunRules starts every declared rule on an instance, once however often it's called.
func TestDeclaredRulesRunOncePerInstance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := vis.NewInt(uniq("test_declared_knob"), vis.Range(1, 512), vis.Default(64), vis.Pull(vis.Rule))
		hot := vis.NewGauge(uniq("test_hot"))
		hot.Set(20)
		vis.NewRule(vis.RuleSpec{Name: uniq("test_declared_rule"), Knob: k, Watch: hot.Measure(), Over: vis.Fixed(5.0),
			For: 3 * time.Second, Step: -16, Cooldown: time.Minute, Lease: time.Hour})
		in := vis.NewInstance()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in.RunRules(ctx)
		in.RunRules(ctx)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := k.Get(in.Snapshot()); got != 48 {
			t.Fatalf("knob = %d, want one step (48): the rule runs once", got)
		}
		cancel()
		synctest.Wait()
	})
}

// A rule can watch a latency gauge with a latency threshold, and not with a number.
func TestARuleCanWatchALatencyGauge(t *testing.T) {
	wait := vis.NewLatency(uniq("test_wait_p99"))
	k := vis.NewInt(uniq("test_lat_knob"), vis.Range(1, 100), vis.Default(50), vis.Pull(vis.Rule))
	r := vis.RuleSpec{Name: uniq("test_lat_rule"), Knob: k, Watch: wait.Measure(), Over: vis.Fixed(400 * time.Millisecond), Step: -5, Lease: time.Minute}
	if p := panicOf(func() { vis.NewRule(r) }); p != "" {
		t.Fatalf("a latency threshold on a latency gauge panicked: %s", p)
	}
	r.Name, r.Over = uniq("test_lat_rule"), vis.Fixed(400)
	if p := panicOf(func() { vis.NewRule(r) }); !strings.Contains(p, "latency") {
		t.Fatalf("a number threshold on a latency gauge: %q, want it refused", p)
	}
	if m := wait.Measure(); m.Unit != "ms" {
		t.Fatalf("a latency gauge's measure is in %q, want ms", m.Unit)
	}
}

// A Duration knob counts nanoseconds, so a bare step or drift in the
// single digits was meant in another unit: NewRule says so.
func TestADurationKnobsStepIsNotNanoseconds(t *testing.T) {
	d := vis.NewDuration(uniq("test_dur_knob"), vis.Range(time.Millisecond, time.Second), vis.Default(100*time.Millisecond), vis.Pull(vis.Rule))
	base := vis.RuleSpec{Knob: d, Watch: vis.SchedP99, Over: vis.Fixed(5 * time.Millisecond), Lease: time.Minute}
	r := base
	r.Name, r.Step = uniq("test_dur_rule"), 5
	if p := panicOf(func() { vis.NewRule(r) }); !strings.Contains(p, "5ns") || !strings.Contains(p, "time.Millisecond") {
		t.Fatalf("a 5 ns step: %q, want it refused, saying how to write it", p)
	}
	r = base
	r.Name, r.Step, r.MaxDrift = uniq("test_dur_rule"), int64(10*time.Millisecond), 10
	if p := panicOf(func() { vis.NewRule(r) }); !strings.Contains(p, "MaxDrift") {
		t.Fatalf("a 10 ns drift: %q, want it refused", p)
	}
	r = base
	r.Name, r.Step = uniq("test_dur_rule"), int64(10*time.Millisecond)
	if p := panicOf(func() { vis.NewRule(r) }); p != "" {
		t.Fatalf("a 10 ms step panicked: %s", p)
	}
}
