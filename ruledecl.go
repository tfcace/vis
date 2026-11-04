package vis

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/tfcace/vis/internal/spec"
)

// Level is a rule's threshold or dead band: a fixed amount (Fixed), or a knob
// people can tune, read on every check. A latency, a time.Duration or a
// Duration knob, goes with a measure in milliseconds, such as SchedP99 or a
// route's p99; a plain number goes with a gauge or a ratio.
type Level interface {
	level(*Config) (v float64, latency bool)
}

type fixed struct {
	v       float64
	latency bool
}

func (f fixed) level(*Config) (float64, bool) { return f.v, f.latency }

// Fixed is a level that never changes: vis.Fixed(400*time.Millisecond) for a
// latency, vis.Fixed(0.9) for a ratio.
func Fixed[T time.Duration | float64 | int](v T) Level {
	switch v := any(v).(type) {
	case time.Duration:
		return fixed{msf(v), true}
	case int:
		return fixed{float64(v), false}
	default:
		return fixed{any(v).(float64), false}
	}
}

func (k Duration) level(c *Config) (float64, bool) { return msf(k.Get(c)), true }
func (k Int) level(c *Config) (float64, bool)      { return float64(k.Get(c)), false }

// isLatency is whether a measure is a latency.
func (m Measure) isLatency() bool { return m.kindOf() == spec.Latency }

// check is everything a declared rule must get right, with how to fix each.
func (r RuleSpec) check() error {
	kind := func(latency bool) string {
		if latency {
			return "a latency (a time.Duration, or a Duration knob)"
		}
		return "a plain number (not a time.Duration)"
	}
	switch {
	case r.Name == "":
		return fmt.Errorf("a rule needs a Name")
	case r.Knob == nil:
		return fmt.Errorf("rule %q needs a Knob to move", r.Name)
	case !r.Knob.decl().pulls(Rule):
		return fmt.Errorf("rule %q: %s doesn't let rules change it; declare vis.Pull(vis.Rule) on the knob", r.Name, r.knob())
	case r.Watch.Read == nil:
		return fmt.Errorf("rule %q needs a Watch, such as vis.SchedP99 or a gauge's Measure()", r.Name)
	case r.Over == nil:
		return fmt.Errorf("rule %q needs Over, its threshold, such as vis.Fixed(400*time.Millisecond)", r.Name)
	case r.Step == 0 && r.Do == nil:
		return fmt.Errorf("rule %q needs a Step (how far to move %s each time, in its own unit), or a Do: vis.StepPercent(p), vis.SetTo(v) or vis.Reset()", r.Name, r.knob())
	case r.Step != 0 && r.Do != nil:
		return fmt.Errorf("rule %q has a Step and a Do: a rule takes one action", r.Name)
	case r.Lease <= 0:
		return fmt.Errorf("rule %q needs a Lease: how long each step lasts", r.Name)
	}
	d := r.Knob.decl()
	if d.isDuration {
		for _, f := range []struct {
			field string
			v     int64
		}{{"Step", r.Step}, {"MaxDrift", r.MaxDrift}} {
			if f.v != 0 && max(f.v, -f.v) < int64(spec.MinDurationStep) {
				return fmt.Errorf("rule %q: a %s of %d on %s, a Duration knob, is %v: it counts nanoseconds, so write it as a duration, such as int64(5*time.Millisecond)",
					r.Name, f.field, f.v, r.knob(), time.Duration(f.v))
			}
		}
	}
	if w := d.max - d.min; r.Step > w || -r.Step > w {
		return fmt.Errorf("rule %q: a Step of %d is wider than %s's range %s..%s", r.Name, r.Step, r.knob(), d.format(d.min), d.format(d.max))
	}
	if a, ok := r.Do.(stepPercent); ok {
		switch {
		case a.p == 0 || math.IsNaN(a.p):
			return fmt.Errorf("rule %q: StepPercent(%g) never moves %s", r.Name, a.p, r.knob())
		case a.p <= -100:
			return fmt.Errorf("rule %q: StepPercent(%g) cuts %s to nothing in one step; give it more than -100%%, such as -25", r.Name, a.p, r.knob())
		}
	}
	if a, ok := r.Do.(setTo); ok {
		want := "an integer"
		if d.isDuration {
			want = "a time.Duration"
		}
		switch {
		case !d.fits(a.t):
			return fmt.Errorf("rule %q: SetTo was given %v; %s needs %s", r.Name, a.t, r.knob(), want)
		case a.v < d.min || a.v > d.max:
			return fmt.Errorf("rule %q: SetTo(%s) is outside %s's range %s..%s", r.Name, d.format(a.v), r.knob(), d.format(d.min), d.format(d.max))
		}
	}
	if _, lat := r.Over.level(nil0()); lat != r.Watch.isLatency() {
		return fmt.Errorf("rule %q: %s is %s, so its threshold Over must be one too", r.Name, r.Watch.Name, kind(r.Watch.isLatency()))
	}
	if r.Deadband != nil {
		if _, lat := r.Deadband.level(nil0()); lat != r.Watch.isLatency() {
			return fmt.Errorf("rule %q: %s is %s, so its dead band must be one too", r.Name, r.Watch.Name, kind(r.Watch.isLatency()))
		}
	}
	return nil
}

// Action is what a rule does instead of a fixed Step: StepPercent moves its
// knob by a share of its value, SetTo puts it at one value, Reset ends what
// rules set on it.
type Action interface{ action() }

type stepPercent struct{ p float64 }

type setTo struct {
	v int64
	t reflect.Type
}

type resetAction struct{}

func (stepPercent) action() {}
func (setTo) action()       {}
func (resetAction) action() {}

// StepPercent is a rule's action that moves its knob by p percent of its
// current value each time: StepPercent(-25) takes a batch of 64 to 48, then
// 36, 27 and 20. Sizes, counts and concurrency respond to ratios, so a
// percent step is as gentle near the bottom of the range as at the top, where
// a fixed step of -16 takes 16 to 1. An Int knob moves by at least one.
func StepPercent(p float64) Action { return stepPercent{p} }

// SetTo is a rule's action that puts its knob at v: an integer for an Int
// knob, a time.Duration for a Duration knob, inside its range.
func SetTo[T ~int | ~int64](v T) Action { return setTo{int64(v), reflect.TypeFor[T]()} }

// Reset is a rule's action that ends the value rules hold on its knob, so it
// falls back to the value below them: one rule can undo another's.
func Reset() Action { return resetAction{} }

// nil0 is a snapshot of every knob at its default, for checking a level's kind
// when no instance exists yet.
func nil0() *Config {
	ds := allDecls()
	c := &Config{vals: make([]int64, len(ds))}
	for i, d := range ds {
		c.vals[i] = d.def
	}
	return c
}

// DeclaredRule is a rule declared with NewRule.
type DeclaredRule struct{ spec RuleSpec }

var declaredRules struct {
	sync.Mutex
	rules []*DeclaredRule
	names map[string]bool
}

// NewRule declares a rule at package level, next to the knob it moves. It is
// checked now, and panics saying what to fix: a knob that doesn't let rules
// change it, a threshold in the wrong unit for what it watches, no step or
// lease. vis.Start runs it on the default instance; RunRules runs every
// declared rule on another one.
func NewRule(r RuleSpec) *DeclaredRule {
	if err := r.check(); err != nil {
		panic("vis: " + err.Error())
	}
	declaredRules.Lock()
	defer declaredRules.Unlock()
	if declaredRules.names == nil {
		declaredRules.names = map[string]bool{}
	}
	if declaredRules.names[r.Name] {
		panic(fmt.Sprintf("vis: rule %q is declared twice", r.Name))
	}
	declaredRules.names[r.Name] = true
	rule := &DeclaredRule{r}
	declaredRules.rules = append(declaredRules.rules, rule)
	return rule
}

// Name is the rule's name.
func (r *DeclaredRule) Name() string { return r.spec.Name }

// RunRules runs every declared rule on in until ctx ends, each on its own
// goroutine. It starts them once per instance, however often it's called.
func (in *Instance) RunRules(ctx context.Context) {
	declaredRules.Lock()
	rules := append([]*DeclaredRule(nil), declaredRules.rules...)
	declaredRules.Unlock()
	in.rules.mu.Lock()
	if in.rules.declared == nil {
		in.rules.declared = map[string]bool{}
	}
	var start []RuleSpec
	for _, r := range rules {
		if !in.rules.declared[r.spec.Name] {
			in.rules.declared[r.spec.Name] = true
			start = append(start, r.spec)
		}
	}
	in.rules.mu.Unlock()
	for _, r := range start {
		go in.RunRule(ctx, r)
	}
}
