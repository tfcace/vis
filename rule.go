package vis

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// RuleSpec is a compiled-in reflex: when what it watches stays Over its threshold for For, it
// steps Knob by Step, at most once per Cooldown, under a lease. It carries these
// safety guards: a rule whose steps don't improve its signal stops
// (Stall), it moves the knob at most MaxDrift from the value below its layer,
// it can run in Shadow, and a freeze holds it.
//
// A rule acts on its signal and answers to its Objective: a step that quiets
// the signal while the objective gets worse means the rule is pushing the
// wrong lever, so vis undoes its change and stops it until a person or an
// agent switches it back on. While a person's or an agent's change is being
// measured, rules hold off, so their steps don't confound it.
type RuleSpec struct {
	Name     string
	Knob     Knob          // the knob it moves: the handle itself, not its name
	Watch    Measure       // what it watches, such as vis.SchedP99, vis.RouteP99(route) or a gauge's Measure()
	Over     Level         // its threshold: vis.Fixed(5*time.Millisecond), or a knob people can tune
	For      time.Duration // how long what it watches must stay over the threshold before acting
	Step     int64         // how far to move the knob per action, in the knob's own unit
	Do       Action        // instead of Step: vis.SetTo(v) puts the knob at v; vis.Reset() ends what rules set on it
	Cooldown time.Duration // the least time between actions, and how long a step gets to show its effect
	Lease    time.Duration // how long each step lasts
	Deadband Level         // the least improvement that counts as helping, in Over's unit; none by default
	Stall    int           // actions in a row that may fail to help before the rule stops; default 2
	MaxDrift int64         // total distance from the value below the rule's layer; default four steps
	Shadow   bool          // record what it would do, without doing it
	Every    time.Duration // how often to check; default 1s

	// Objective is what the rule answers to, lower being better: by default the
	// first SLO's p99. Name another for a rule that trades latency on purpose,
	// such as one that sheds load to save memory.
	Objective Measure
}

func (r RuleSpec) knob() string { return r.Knob.decl().name }

// RuleState is what a rule is doing, for people, agents and the UI.
type RuleState struct {
	Name       string  `json:"name"`
	Knob       string  `json:"knob"`
	SignalName string  `json:"signal_name"`
	Unit       string  `json:"unit,omitempty"`
	State      string  `json:"state"`  // watching, acting, holding, stalled, stopped, at_limit, aside, overridden, frozen, shadow, disabled, error
	Stalls     int     `json:"stalls"` // actions in a row that didn't help
	Signal     float64 `json:"signal"` // the latest reading of what it watches
	Limit      float64 `json:"limit"`
	Warning    string  `json:"warning,omitempty"` // a reason to doubt the rule can help, found from its declarations
	Err        string  `json:"err,omitempty"`     // what went wrong, in the error state
	Moves      string  `json:"moves,omitempty"`   // where it may take its knob: "1..64", from the value below it and MaxDrift
	Action     string  `json:"action,omitempty"`  // what it does: "step -25%", "step -16", "set 8", "reset"
	Stopped    string  `json:"stopped,omitempty"` // why vis stopped it, until a person or an agent switches it back on
	Aside      string  `json:"aside,omitempty"`   // in the aside state, why: its knob is at its special value, which no rule moves
}

// warning checks a rule against the declarations: its knob must let rules
// write it, and a rule watching a CPU measure should move a knob that spends CPU.
func (r RuleSpec) warning() string {
	if !r.Knob.decl().pulls(Rule) {
		return fmt.Sprintf("%s doesn't let rules change it, so this rule can't act. Declare vis.Pull(vis.Rule) on the knob.", r.knob())
	}
	res := r.Watch.Resource
	if res == "" {
		return ""
	}
	for _, s := range r.Knob.decl().spends {
		if s.r() == res {
			return ""
		}
	}
	cost := map[Resource]string{CPU: "vis.CPU", Memory: "vis.Memory"}[res] // a gauge is named by its own variable
	if cost == "" {
		cost = "the " + string(res) + " gauge"
	}
	return fmt.Sprintf("%s doesn't spend %s, which %s measures: this rule may not be able to help. If turning it up uses %[2]s, declare vis.Uses(%[4]s) on the knob.",
		r.knob(), res, r.Watch.Name, cost)
}

type ruleTable struct {
	mu       sync.Mutex
	rules    map[string]*RuleState
	specs    map[string]RuleSpec // running rules, for SetRule
	disabled map[string]bool
	stopped  map[string]string // rules vis stopped, and why
	declared map[string]bool   // declared rules RunRules started
}

// SetRule switches a running rule off or on. Disabling it also ends its lease,
// so its knob falls back now instead of keeping the rule's last value. People
// and agents may do this, with a reason; during a freeze only a person may,
// since ending the lease is a change.
func (in *Instance) SetRule(name string, enabled bool, by Writer, reason string) error {
	if !by.valid() {
		return ErrActorRequired
	}
	if by.Kind != Human && by.Kind != Agent {
		return fmt.Errorf("%w: only people and agents switch rules (%v)", ErrNotAllowed, by)
	}
	t := &in.rules
	t.mu.Lock()
	spec, ok := t.specs[name]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownRule, name)
	}
	in.mu.Lock()
	if in.freeze.Frozen && by.Kind != Human {
		in.mu.Unlock()
		return fmt.Errorf("%w (by %v)", ErrFrozen, in.freeze.By)
	}
	t.mu.Lock()
	if t.disabled == nil {
		t.disabled = map[string]bool{}
	}
	t.disabled[name] = !enabled
	delete(t.stopped, name)           // switching a rule either way is a person's or an agent's decision about it
	if s := t.rules[name]; s != nil { // the reply says what it is now; the rule's loop catches up on its next check
		s.State, s.Stalls = "watching", 0
		if !enabled {
			s.State = "disabled"
		}
	}
	t.mu.Unlock()
	event := "enabled"
	if !enabled {
		event = "disabled"
		in.endRuleLease(spec, by, "rule "+name+" disabled: "+reason)
	}
	in.audit(slog.LevelInfo, "vis.rule", slog.String("rule", name), slog.String("knob", spec.knob()), slog.String("event", event),
		slog.String("actor", by.String()), slog.String("reason", reason))
	in.mu.Unlock()
	in.notify()
	return nil
}

// endRuleLayer ends whatever value rules hold on knob, so it falls back to the
// value below them. Callers hold in.mu and notify after.
func (in *Instance) endRuleLayer(knob string, by Writer, reason string) {
	if d := lookup(knob); d != nil && d.idx < len(in.knobs) {
		if s := in.knobs[d.idx].layers[RuleLayer]; s.set && s.lease != nil {
			in.end(s.lease, []slog.Attr{slog.String("op", "revert"), slog.String("actor", by.String()), slog.String("reason", reason)})
		}
	}
}

// endRuleLease ends the rule's lease on its knob, if it holds one, so the knob
// falls back to the value below the rule. Callers hold in.mu and notify after.
func (in *Instance) endRuleLease(spec RuleSpec, by Writer, reason string) {
	if d := lookup(spec.knob()); d != nil && d.idx < len(in.knobs) {
		if s := in.knobs[d.idx].layers[RuleLayer]; s.set && s.lease != nil && s.by == (Writer{Kind: Rule, ID: spec.Name}) {
			in.end(s.lease, []slog.Attr{slog.String("op", "revert"), slog.String("actor", by.String()), slog.String("reason", reason)})
		}
	}
}

func (t *ruleTable) isDisabled(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.disabled[name]
}

func (t *ruleTable) stoppedWhy(name string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped[name]
}

// Rules reports every running rule's state.
func (in *Instance) Rules() []RuleState {
	t := &in.rules
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []RuleState
	for _, s := range t.rules {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b RuleState) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// RunRule runs one rule on this instance, checking it every r.Every until ctx
// is done. A rule declared with NewRule needs none of this: vis.Start, or
// RunRules on another instance, runs it. RunRule is for a rule built at run
// time, as tests do; it doesn't check the spec the way NewRule does.
func (in *Instance) RunRule(ctx context.Context, r RuleSpec) {
	if r.Stall <= 0 {
		r.Stall = 2
	}
	if r.Every <= 0 {
		r.Every = time.Second
	}
	rr := &ruleRun{in: in, spec: r, by: Writer{Kind: Rule, ID: r.Name}}
	in.rules.mu.Lock()
	if in.rules.specs == nil {
		in.rules.specs = map[string]RuleSpec{}
	}
	in.rules.specs[r.Name] = r
	in.rules.mu.Unlock()
	if w := r.warning(); w != "" {
		in.logger().Warn("vis: rule "+r.Name+": "+w, "rule", r.Name)
	}
	rr.moves = rr.reach()
	in.logger().Info("vis: rule "+r.Name+" may move "+r.knob()+" within "+rr.moves, "rule", r.Name)
	v, lim, _ := rr.read() // so "started" records the threshold it starts with
	rr.set("watching", "started", v, lim)
	rr.check(time.Now())
	t := time.NewTicker(r.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			rr.check(now)
		}
	}
}

type ruleRun struct {
	in   *Instance
	spec RuleSpec
	by   Writer

	state            string
	stalls           int
	overSince, under time.Time // when the signal went over the limit, and under it
	quietUntil       time.Time // the cooldown after an action
	acted            bool      // whether lastSignal belongs to an action still being judged
	lastSignal       float64
	judge            *judgment // the last step, until it's judged against the objective
	err              string    // what went wrong, in the error state
	readFailed       bool      // whether that was reading the signal, which clears once a read works
	stepAt           time.Time // the last step, so what's watched after it counts only what came since
	idle             int       // steps in a row that quieted the signal but left the objective broken, no closer to it
	moves            string    // where it may take its knob, as RuleState.Moves says
}

// read takes the rule's signal and limit. Both are app code: a panic is
// reported as the rule's error rather than taking the rule, and the process, down.
func (rr *ruleRun) read() (v, lim float64, err string) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Sprintf("reading %s or its limit panicked: %v", cmp.Or(rr.spec.Watch.Name, rr.spec.Name), p)
		}
	}()
	lim, _ = rr.spec.Over.level(rr.in.Snapshot())
	return rr.watch(), lim, ""
}

// limit is the rule's threshold now, for a rule that isn't reading its signal:
// switched off or stopped, it still says what it would act on.
func (rr *ruleRun) limit() (lim float64) {
	defer func() { recover() }() // a limit that panics is reported when the rule runs again
	lim, _ = rr.spec.Over.level(rr.in.Snapshot())
	return lim
}

// watch reads what the rule watches. After a step, a route's p99 counts only
// the requests served since, so a minute mostly from before the step doesn't
// judge it.
func (rr *ruleRun) watch() float64 {
	if w := rr.spec.Watch; w.since != nil && !rr.stepAt.IsZero() && time.Since(rr.stepAt) < time.Minute {
		if v, ok := w.since(rr.in, rr.stepAt); ok {
			return v
		}
	}
	return rr.spec.Watch.Read(rr.in)
}

// deadband is the least improvement that counts as helping, now.
func (rr *ruleRun) deadband() float64 {
	if rr.spec.Deadband == nil {
		return 0
	}
	v, _ := rr.spec.Deadband.level(rr.in.Snapshot())
	return v
}

// fail puts the rule in the error state with the reason, audited once per new reason.
func (rr *ruleRun) fail(why string, v, lim float64) {
	if rr.state == "error" && rr.err == why {
		rr.observe(v, lim)
		return
	}
	rr.err, rr.state = why, "" // a new reason is worth recording
	rr.set("error", "error", v, lim)
}

// judgment is a step waiting to be judged against the objective once its cooldown is over.
type judgment struct {
	at, due           time.Time // the step, and when to judge it
	signal, objective float64
	failed            float64 // the objective route's failed share before the step
	failedOK          bool    // and there was one to read
}

// failedBy is how far a step may raise the share of the objective's requests
// that fail (5xx) before it counts as harm, as a rollout's gate counts errors
// first: a request that fails fast pulls p99 down, so p99 alone reads it as a win.
const failedBy = .10

// failedShare is the share of the objective route's requests that failed, over
// the rule's cooldown, or since a step. ok is false for a rule that answers to
// a measure of its own, or with no requests.
func (rr *ruleRun) failedShare(since time.Time) (route string, share float64, ok bool) {
	if rr.spec.Objective.Read != nil {
		return "", 0, false
	}
	objs := rr.in.objectives()
	if len(objs) == 0 {
		return "", 0, false
	}
	route = objs[0].route
	r := rr.in.Route(route, max(rr.spec.Cooldown, bucketWidth))
	if !since.IsZero() {
		if after := rr.in.routes.since(route, since, time.Now()); after.Requests > 0 {
			r = after
		}
	}
	if r.Requests == 0 {
		return route, 0, false
	}
	return route, float64(r.Errors) / float64(r.Requests), true
}

// worseBy is how much worse the objective must get after a step, and past the SLO, to count as the step's harm.
const worseBy = 1.5

// objective reads what the rule answers to now: its name, unit, value, and the
// level below which it isn't harm (the SLO's target). ok is false with nothing to judge by.
func (rr *ruleRun) objective(since time.Time) (name, unit string, v, floor float64, ok bool) {
	if o := rr.spec.Objective; o.Read != nil {
		return o.Name, o.Unit, o.Read(rr.in), 0, true
	}
	objs := rr.in.objectives()
	if len(objs) == 0 {
		return "", "", 0, 0, false
	}
	s := objs[0]
	r := rr.in.Route(s.route, max(rr.spec.Cooldown, bucketWidth))
	if !since.IsZero() { // judging a step: only the requests served since it
		if after := rr.in.routes.since(s.route, since, time.Now()); after.Requests > 0 {
			r = after
		}
	}
	return s.route + " p99", "ms", msf(r.P99), msf(s.p99), r.Requests > 0
}

func (rr *ruleRun) check(now time.Time) {
	r := rr.spec
	if rr.in.rules.isDisabled(r.Name) {
		if rr.state != "disabled" {
			rr.stalls, rr.acted, rr.overSince = 0, false, time.Time{}
			rr.set("disabled", "", 0, rr.limit()) // SetRule audited who and why
		}
		return
	}
	if why := rr.in.rules.stoppedWhy(r.Name); why != "" {
		if rr.state != "stopped" {
			rr.set("stopped", "", 0, rr.limit())
		}
		return
	}
	if rr.state == "disabled" || rr.state == "stopped" { // switched back on
		rr.stalls, rr.acted, rr.judge, rr.idle, rr.overSince = 0, false, nil, 0, time.Time{}
		rr.set("watching", "", 0, 0)
	}
	v, lim, err := rr.read()
	if err != "" {
		rr.fail(err, 0, 0)
		rr.readFailed = true
		return
	}
	readFailed := rr.readFailed
	rr.readFailed = false
	if rr.state == "error" && readFailed { // the read works again; a failed step stays until a step succeeds
		rr.set("watching", "", v, lim)
	}
	rr.observe(v, lim)

	// Judge the last step against the objective, whether or not the signal is still over its limit.
	// Where it left the objective is where a step taken now starts from, so no
	// step is credited with what the one before it did.
	var left *float64
	if j := rr.judge; j != nil && !now.Before(j.due) {
		rr.judge = nil
		name, unit, after, floor, ok := rr.objective(j.at)
		if ok {
			left = &after
		}
		gain := j.signal - v
		signal := fmt.Sprintf("its signal %s improved (%s → %s)", cmp.Or(r.Watch.Name, r.Name), measured(j.signal, r.Watch.Unit), measured(v, r.Watch.Unit))
		if gain <= 0 {
			signal = fmt.Sprintf("its signal %s went %s → %s", cmp.Or(r.Watch.Name, r.Name), measured(j.signal, r.Watch.Unit), measured(v, r.Watch.Unit))
		}
		if route, failed, fok := rr.failedShare(j.at); fok && j.failedOK && failed-j.failed >= failedBy && !rr.othersChanged(j.at) {
			fx := moved(route+" failed", math.Round(j.failed*100), math.Round(failed*100), "%")
			rr.stop(fmt.Sprintf("%s but %s after its step", signal, fx), v, lim, "failed",
				judged(cmp.Or(r.Watch.Name, r.Name), j.signal, v, r.Watch.Unit), fx)
			return
		}
		switch {
		case !ok || gain <= 0 || gain < rr.deadband() || rr.othersChanged(j.at):
			// nothing to say about the objective: the signal didn't move (the stall check's case), or someone else changed something
		case after > floor && after >= j.objective*worseBy:
			rr.stop(fmt.Sprintf("%s but %s got worse (%s → %s) after its step", signal, name, measured(j.objective, unit), measured(after, unit)), v, lim, "worse",
				judged(cmp.Or(r.Watch.Name, r.Name), j.signal, v, r.Watch.Unit), judged(name, j.objective, after, unit))
			return
		case after > floor && !closer(j.objective, after, floor):
			if rr.idle++; rr.idle >= r.Stall {
				rr.stop(fmt.Sprintf("%s but %s stayed over its %s objective, no closer to it (%s → %s), after %d steps in a row",
					signal, name, measured(floor, unit), measured(j.objective, unit), measured(after, unit), rr.idle), v, lim, "no_closer",
					judged(cmp.Or(r.Watch.Name, r.Name), j.signal, v, r.Watch.Unit), judged(name, j.objective, after, unit))
				return
			}
		default:
			rr.idle = 0
		}
	}

	if v <= lim { // the condition cleared
		rr.overSince = time.Time{}
		if rr.under.IsZero() {
			rr.under = now
		}
		if now.Sub(rr.under) >= r.For && rr.state != "watching" {
			rr.stalls, rr.acted = 0, false
			rr.set("watching", "cleared", v, lim)
		}
		return
	}
	rr.under = time.Time{}
	if rr.overSince.IsZero() {
		rr.overSince = now
	}
	if now.Sub(rr.overSince) < r.For || rr.state == "stalled" || now.Before(rr.quietUntil) {
		return
	}
	if !r.Shadow {
		if change := rr.in.measuring(); change != 0 {
			rr.acted, rr.judge = false, nil // someone else's change makes its last step's effect unreadable: judge afresh once it may act
			rr.set("holding", "holding", v, lim)
			return
		}
	}

	// Judge the last action: did the signal improve by at least the dead band since?
	if rr.acted {
		if rr.lastSignal-v >= rr.deadband() {
			rr.stalls = 0
		} else {
			rr.stalls++
		}
		if rr.stalls >= r.Stall {
			rr.set("stalled", "stalled", v, lim)
			return
		}
	}

	mv, why := rr.allowedMove()
	if why != "" {
		rr.set(why, why, v, lim)
		return
	}
	reason := fmt.Sprintf("%s %.3g%s > %.3g%s", cmp.Or(r.Watch.Name, r.Name), v, r.Watch.Unit, lim, r.Watch.Unit)
	if r.Shadow {
		rr.in.audit(slog.LevelInfo, "vis.rule", slog.String("rule", r.Name), slog.String("knob", r.knob()),
			slog.String("event", "would_"+mv.verb()), slog.Int64("by", mv.step), slog.String("signal_name", r.Watch.Name), slog.Float64("signal", v), slog.Float64("limit", lim), slog.String("unit", r.Watch.Unit))
		rr.set("shadow", "", v, lim) // published, so the UI and agents read what the history says; would_ above is its audit
	} else {
		var err error
		switch {
		case mv.reset:
			rr.in.mu.Lock()
			rr.in.endRuleLayer(r.knob(), rr.by, "rule "+r.Name+" reset it: "+reason)
			rr.in.mu.Unlock()
			rr.in.notify()
		case mv.set != nil:
			_, err = rr.in.Apply(Tx{By: rr.by, For: r.Lease, Reason: reason, Set: []Write{{Knob: r.knob(), Value: *mv.set}}})
		default:
			_, err = rr.in.Apply(Tx{By: rr.by, For: r.Lease, Reason: reason, Step: []Step{{Knob: r.knob(), By: mv.step}}})
		}
		switch {
		case errors.Is(err, ErrFrozen):
			rr.set("frozen", "frozen", v, lim) // a freeze is not the rule failing: no stall counted
			return
		case err != nil:
			rr.fail(err.Error(), v, lim)
			return
		}
		rr.set("acting", "", v, lim)
		if _, _, o, _, ok := rr.objective(time.Time{}); ok {
			if left != nil { // the last step's judgment just read it: the requests since that step, not a window reaching back past it
				o = *left
			}
			rr.judge = &judgment{at: now, due: now.Add(r.Cooldown), signal: v, objective: o}
			_, rr.judge.failed, rr.judge.failedOK = rr.failedShare(time.Time{})
		}
	}
	rr.acted, rr.lastSignal, rr.stepAt = true, v, now
	rr.quietUntil, rr.overSince = now.Add(r.Cooldown), time.Time{}
}

// reach is where the rule may take its knob: from the value below its layer,
// MaxDrift in the direction it steps, inside the knob's range.
func (rr *ruleRun) reach() string {
	d := rr.spec.Knob.decl()
	switch a := rr.spec.Do.(type) {
	case setTo:
		return d.format(a.v)
	case resetAction:
		return "back to the value below the rules"
	}
	base := d.def
	if st, err := rr.in.Stack(rr.spec.knob()); err == nil {
		base = st[len(st)-1].Value
		for _, e := range st {
			if e.Layer < RuleLayer {
				base = e.Value
				break
			}
		}
	}
	if d.isSpecial(base) {
		return "" // it stands aside: no step from a special value
	}
	lo, hi := rr.spec.bounds(base)
	if rr.spec.down() {
		hi = base
	} else {
		lo = base
	}
	return d.format(lo) + ".." + d.format(hi)
}

// next is where one step takes the knob from cur: by Step, or by StepPercent
// of cur, rounded, and by at least one unit.
func (r RuleSpec) next(cur int64) int64 {
	a, ok := r.Do.(stepPercent)
	if !ok {
		return cur + r.Step
	}
	to := int64(math.Round(float64(cur) * (1 + a.p/100)))
	switch {
	case to == cur && a.p < 0:
		to--
	case to == cur:
		to++
	}
	return to
}

// down is whether the rule's steps lower its knob.
func (r RuleSpec) down() bool {
	a, ok := r.Do.(stepPercent)
	return r.Step < 0 || ok && a.p < 0
}

// bounds is how far the rule may take its knob from base, the value below its
// layer: MaxDrift either way, by default as far as four steps go, inside the
// knob's range.
func (r RuleSpec) bounds(base int64) (lo, hi int64) {
	drift := r.MaxDrift
	if drift <= 0 {
		v := base
		for range 4 {
			v = r.next(v)
		}
		drift = max(v-base, base-v)
	}
	d := r.Knob.decl()
	return max(base-drift, d.min), min(base+drift, d.max)
}

// action says what the rule does, for people and agents.
func (rr *ruleRun) action() string {
	d := rr.spec.Knob.decl()
	switch a := rr.spec.Do.(type) {
	case setTo:
		return "set " + d.format(a.v)
	case resetAction:
		return "reset"
	case stepPercent:
		return fmt.Sprintf("step %g%%", a.p)
	}
	if d.isDuration {
		return "step " + time.Duration(rr.spec.Step).String()
	}
	return fmt.Sprintf("step %d", rr.spec.Step)
}

// othersChanged is whether anyone but this rule changed a knob since t: a
// change in the objective since then can't be laid at the rule's door.
func (rr *ruleRun) othersChanged(t time.Time) bool {
	for _, e := range rr.in.History(MaxHistory) {
		if e.Kind == "change" && e.At.After(t) && fmt.Sprint(e.Fields["actor"]) != rr.by.String() {
			return true
		}
	}
	return false
}

// move is one action a rule takes: a step, a value to set, or a reset.
type move struct {
	step  int64
	set   *int64
	reset bool
}

func (m move) verb() string {
	switch {
	case m.reset:
		return "reset"
	case m.set != nil:
		return "set"
	}
	return "step"
}

// allowedMove is what the rule may do now, or why it can't: its step, its
// value unless the knob is at it already, or a reset if rules hold a value.
func (rr *ruleRun) allowedMove() (move, string) {
	switch a := rr.spec.Do.(type) {
	case nil, stepPercent:
		step, why := rr.allowedStep()
		return move{step: step}, why
	case setTo:
		st, err := rr.in.Stack(rr.spec.knob())
		switch {
		case err != nil || len(st) == 0:
			return move{}, "error"
		case st[0].Layer > RuleLayer:
			return move{}, "overridden"
		case rr.spec.Knob.decl().isSpecial(st[0].Value):
			return move{}, "aside"
		case st[0].Value == a.v:
			return move{}, "at_limit"
		}
		v := a.v
		return move{set: &v}, ""
	case resetAction:
		st, err := rr.in.Stack(rr.spec.knob())
		if err != nil {
			return move{}, "error"
		}
		for _, e := range st {
			if e.Layer == RuleLayer {
				return move{reset: true}, ""
			}
		}
		return move{}, "at_limit" // nothing a rule set to undo
	}
	return move{}, "error"
}

// allowedStep is the step the rule may take now, or why it can't: a person's
// pin sits above it, or the knob is already MaxDrift from the value below the rule.
func (rr *ruleRun) allowedStep() (int64, string) {
	r := rr.spec
	st, err := rr.in.Stack(r.knob())
	if err != nil || len(st) == 0 {
		return 0, "error"
	}
	if st[0].Layer > RuleLayer {
		return 0, "overridden"
	}
	cur, base := st[0].Value, st[len(st)-1].Value
	for _, e := range st {
		if e.Layer < RuleLayer {
			base = e.Value
			break
		}
	}
	if d := r.Knob.decl(); d.isSpecial(cur) || d.isSpecial(base) { // nothing to step from, and no reach around it
		return 0, "aside"
	}
	lo, hi := r.bounds(base)
	if r.down() && cur <= lo || !r.down() && cur >= hi {
		return 0, "at_limit"
	}
	to := min(max(r.next(cur), lo), hi)
	return to - cur, ""
}

// stop undoes the rule's change and stops it: its step quieted its signal while
// its objective got worse. It stays stopped until someone switches it back on.
// closer is whether a step brought a broken objective a good part of the
// way back: a quarter of the gap to its target, or under it.
func closer(before, after, target float64) bool {
	return after <= target || before > target && before-after >= (before-target)/4
}

func (rr *ruleRun) stop(why string, v, lim float64, verdict string, fx ...Moved) {
	t := &rr.in.rules
	t.mu.Lock()
	if t.stopped == nil {
		t.stopped = map[string]string{}
	}
	t.stopped[rr.spec.Name] = why
	t.mu.Unlock()
	rr.stalls, rr.acted, rr.idle, rr.overSince = 0, false, 0, time.Time{}
	rr.set("stopped", "", v, lim)
	in := rr.in
	in.mu.Lock()
	in.audit(slog.LevelWarn, "vis.rule", slog.String("rule", rr.spec.Name), slog.String("knob", rr.spec.knob()),
		slog.String("event", "stopped"), slog.String("verdict", verdict), slog.String("reason", why), slog.Any("effects", fx))
	in.endRuleLease(rr.spec, rr.by, "rule "+rr.spec.Name+" stopped: "+why)
	in.mu.Unlock()
	in.notify()
}

// judged is an effect a rule's step had, at the precision measured says it.
func judged(name string, from, to float64, unit string) Moved {
	if unit == "ms" {
		return moved(name, rough(from), rough(to), unit)
	}
	return moved(name, from, to, unit)
}

func measured(v float64, unit string) string {
	if unit == "ms" {
		return roughMs(v) + " ms"
	}
	return strings.TrimSpace(fmt.Sprintf("%.3g %s", v, unit))
}

// set records a state change, and audits it when it is one worth knowing about.
func (rr *ruleRun) set(state, event string, v, lim float64) {
	rr.moves = rr.reach() // the value below the rule may have changed since it started
	var aside string
	if state == "aside" {
		aside = rr.aside()
	}
	changed := state != rr.state
	rr.state = state
	t := &rr.in.rules
	t.mu.Lock()
	if t.rules == nil {
		t.rules = map[string]*RuleState{}
	}
	t.rules[rr.spec.Name] = &RuleState{Name: rr.spec.Name, Knob: rr.spec.knob(), SignalName: rr.spec.Watch.Name, Unit: rr.spec.Watch.Unit,
		State: state, Stalls: rr.stalls, Signal: v, Limit: lim, Warning: rr.spec.warning(), Stopped: t.stopped[rr.spec.Name], Moves: rr.moves, Action: rr.action()}
	t.rules[rr.spec.Name].Aside = aside
	if state == "error" {
		t.rules[rr.spec.Name].Err = rr.err
	} else {
		rr.err = ""
	}
	t.mu.Unlock()
	if changed && event != "" {
		rr.in.audit(slog.LevelInfo, "vis.rule", slog.String("rule", rr.spec.Name), slog.String("knob", rr.spec.knob()),
			slog.String("event", event), slog.String("signal_name", rr.spec.Watch.Name), slog.Float64("signal", v), slog.Float64("limit", lim),
			slog.String("unit", rr.spec.Watch.Unit), slog.Int("stalls", rr.stalls), slog.String("error", rr.err))
	}
}

// aside says why the rule stands aside: its knob, or the value below the
// rule's own, is the knob's special value.
func (rr *ruleRun) aside() string {
	d := rr.spec.Knob.decl()
	v := d.def
	if st, err := rr.in.Stack(rr.spec.knob()); err == nil && len(st) > 0 {
		v = st[0].Value
		for _, e := range st {
			if e.Layer < RuleLayer && d.isSpecial(e.Value) {
				v = e.Value
				break
			}
		}
	}
	return fmt.Sprintf("%s is %s, which no rule steps from", rr.spec.knob(), d.format(v))
}

// observe keeps the reported signal current between state changes.
func (rr *ruleRun) observe(v, lim float64) {
	t := &rr.in.rules
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.rules[rr.spec.Name]; s != nil {
		s.Signal, s.Limit, s.Stalls = v, lim, rr.stalls
	}
}
