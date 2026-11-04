package vis

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Impact is what a change did, measured by vis with nothing declared beyond the
// SLOs: the SLO routes' p99 (the busiest routes' without an SLO), CPU,
// throttling and every published signal, over
// the 20 seconds before the change and the 20 seconds ending once it settled.
// No significance is claimed: one instance, one window. Other changes or
// environment events inside the window are named in Confounded.
type Impact struct {
	Change     uint64             `json:"change"`
	At         time.Time          `json:"at"`
	Settle     time.Duration      `json:"-"`
	MeasuredAt time.Time          `json:"measured_at"` // At plus the settle time
	Done       bool               `json:"done"`
	Changes    []string           `json:"changes"` // "compress_level 9 → 6"
	Before     map[string]float64 `json:"before"`
	After      map[string]float64 `json:"after,omitempty"`
	Confounded []string           `json:"confounded,omitempty"`
	Effects    []Moved            `json:"effects,omitempty"` // what moved, as numbers; Summary says it in a sentence
	Summary    string             `json:"summary"`
	Rest       *Rest              `json:"rest,omitempty"` // in a fleet: the other members over the same windows
	// ConfoundedBy is Confounded with who and where: a member's own rules
	// reacting in the window read differently from a person changing something.
	ConfoundedBy []Confounder `json:"confounded_by,omitempty"`
	// Warming is a change made in the instance's first minute, while its
	// caches and connections still fill: its before window says little.
	Warming bool `json:"warming,omitempty"`
	// Memory is the memory outlook after the change: when the container
	// reaches its limit at the rate since, or that it doesn't within the hour.
	Memory string `json:"memory,omitempty"`
	knobs  []impactKnob
	routes []string      // the routes measured: the SLOs', or the busiest when there are none
	by     string        // who made the change: "agent:oncall", "rule:batch_backoff", "lease"
	p99    float64       // relative change in the first SLO route's p99: -0.97 is 97% faster
	done   chan struct{} // closed once measured
}

// Confounder is something else that changed around a change.
type Confounder struct {
	What  string `json:"what"`
	Kind  string `json:"kind"`            // change, environment, freeze
	Layer string `json:"layer,omitempty"` // a change's layer: rule for what a rule did, its lease ending included
}

// Rest is the rest of the fleet over the same windows as a change made on
// one member: its objectives' p99 from the other members' merged histograms.
// Their holding still while this member moved is what says the change did
// it; their moving too says something they share may have.
type Rest struct {
	Members  []string           `json:"members"`
	Before   map[string]float64 `json:"before"`
	After    map[string]float64 `json:"after,omitempty"`
	Gossiped []string           `json:"gossiped,omitempty"`      // too busy to answer: taken from their gossip
	AfterOf  []string           `json:"after_members,omitempty"` // when the members that answered after differ
	Changed  []string           `json:"changed,omitempty"`       // the rest's own knob changes in the windows: "b: compress_level 9 → 1 by agent:oncall"
}

type impactKnob struct {
	name     string
	from, to int64
}

const (
	warmUp       = time.Minute      // after a start, as the page's "Just started": numbers still settling
	settleFor    = 30 * time.Second // how long a change gets to show its effect
	impactWindow = 20 * time.Second // the window measured before and after
	keepImpacts  = 200
	busiest      = 3                // routes measured when the service has no SLO
	lateMeasure  = 15 * time.Second // how long past measured_at a waiting caller holds on
)

type impactTable struct {
	mu    sync.Mutex
	byID  map[uint64]*Impact
	order []uint64
}

// Impact returns the report for a change: pending until it has settled.
func (in *Instance) Impact(change uint64) (Impact, bool) {
	t := &in.impacts
	t.mu.Lock()
	defer t.mu.Unlock()
	im := t.byID[change]
	if im == nil {
		return Impact{}, false
	}
	return im.copy(), true
}

// copy is a report a caller may keep while it is still being measured: the
// measurement goes on writing the original's Rest, so the copy has its own.
// (Rest's maps and slices are replaced when written, never changed in place.)
func (im *Impact) copy() Impact {
	c := *im
	if im.Rest != nil {
		r := *im.Rest
		c.Rest = &r
	}
	return c
}

// WaitImpact returns a change's report once it has been measured, which is
// the settle time after the change. If ctx ends first it returns the pending
// report with ctx's error.
func (in *Instance) WaitImpact(ctx context.Context, change uint64) (Impact, error) {
	t := &in.impacts
	t.mu.Lock()
	im := t.byID[change]
	t.mu.Unlock()
	if im == nil {
		return Impact{}, in.noMeasurement(change)
	}
	select {
	case <-im.done:
	case <-ctx.Done():
	}
	t.mu.Lock()
	got := im.copy() // from the report itself: it may have left the table meanwhile
	t.mu.Unlock()
	if !got.Done {
		return got, ctx.Err()
	}
	return got, nil
}

// beginImpact records a pending report for a change, then measures the window
// before it on its own goroutine (published signals are app code, and callers
// hold in.mu) and schedules the after.
func (in *Instance) beginImpact(change uint64, at time.Time, knobs []impactKnob, by string) {
	im := &Impact{Change: change, At: at, Settle: settleFor, MeasuredAt: at.Add(settleFor), knobs: knobs, by: by, done: make(chan struct{}),
		Warming: at.Sub(in.started) < warmUp}
	for _, k := range knobs {
		d := lookup(k.name)
		im.Changes = append(im.Changes, fmt.Sprintf("%s %s → %s", k.name, unitValue(d, k.from), unitValue(d, k.to)))
	}
	im.Summary = fmt.Sprintf("Measuring: the effect shows %s after the change.", short(settleFor))
	t := &in.impacts
	t.mu.Lock()
	if t.byID == nil {
		t.byID = map[uint64]*Impact{}
	}
	t.byID[change] = im
	t.order = append(t.order, change)
	if len(t.order) > keepImpacts {
		delete(t.byID, t.order[0])
		t.order = t.order[1:]
	}
	t.mu.Unlock()
	go func() {
		routes := in.measuredRoutes()
		before := in.measureNow(routes)
		var rest *Rest
		if rb, names, gossiped, _ := in.restNow(nil); len(names) > 0 {
			rest = &Rest{Members: names, Before: rb, Gossiped: gossiped}
		}
		t.mu.Lock()
		im.Before, im.routes, im.Rest = before, routes, rest
		t.mu.Unlock()
	}()
	time.AfterFunc(settleFor, func() {
		in.finishImpact(im)
		close(im.done)
	})
}

// measuring is a person's or an agent's change still being measured, or 0:
// while there is one, rules hold off.
func (in *Instance) measuring() uint64 {
	t := &in.impacts
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.order) - 1; i >= 0; i-- {
		im := t.byID[t.order[i]]
		if !im.Done && (strings.HasPrefix(im.by, Human.String()+":") || strings.HasPrefix(im.by, Agent.String()+":")) {
			return im.Change
		}
	}
	return 0
}

// finishImpact measures the after window, even for a report the table has
// dropped meanwhile, so whoever waits on it gets it.
func (in *Instance) finishImpact(im *Impact) {
	t := &in.impacts
	t.mu.Lock()
	routes := im.routes
	t.mu.Unlock()
	after := in.measureNow(routes)
	// The routes after: only requests in buckets that began well after the
	// change, so a backlog draining in its first seconds isn't judged as the
	// change's effect. (A window reaches back to the start of every bucket it
	// touches, which could take in the moment of the change.)
	from := im.At.Add(settleFor - impactWindow)
	for _, route := range routes {
		delete(after, route+" p99")
		delete(after, route+" errors")
		delete(after, route+" refused")
		delete(after, route+" requests")
		delete(after, route+" rps")
		now := time.Now()
		if r := in.routes.since(route, from, now); r.Requests > 0 {
			after[route+" p99"] = msf(r.P99)
			after[route+" errors"] = float64(r.Errors) / float64(r.Requests)
			after[route+" refused"] = float64(r.Refused) / float64(r.Requests)
			after[route+" requests"] = float64(r.Requests)
			if s := in.routes.span(route, 0, from, now); s > 0 {
				after[route+" rps"] = float64(r.Requests) / s.Seconds()
			}
		}
	}
	conf := in.confounders(im.Change, im.At)
	var restAfter map[string]float64
	var afterOf, gossipedAfter, restChanged []string
	t.mu.Lock()
	var restMembers []string
	if im.Rest != nil {
		restMembers = slices.Clone(im.Rest.Members)
	}
	t.mu.Unlock()
	if restMembers != nil { // the same members as before, so the windows compare like with like
		restAfter, afterOf, gossipedAfter, restChanged = in.restNow(restMembers) // their changes cover both windows by now
	}
	t.mu.Lock()
	if restMembers != nil {
		im.Rest.After = restAfter
		if !slices.Equal(afterOf, restMembers) {
			im.Rest.AfterOf = afterOf
		}
		im.Rest.Changed = restChanged
		for _, g := range gossipedAfter {
			if !slices.Contains(im.Rest.Gossiped, g) {
				im.Rest.Gossiped = append(im.Rest.Gossiped, g)
			}
		}
	}
	im.ConfoundedBy = conf
	im.Confounded = nil
	for _, c := range conf {
		im.Confounded = append(im.Confounded, c.What)
	}
	im.After, im.Done = after, true
	im.Memory = memoryOutlook(in.Runtime(impactWindow), impactWindow) // the rate after the change
	im.Effects, im.p99 = in.effects(im)
	im.Summary = in.summarize(im)
	t.mu.Unlock()
}

// measuredRoutes are the routes a change is judged by: the SLOs', or, for a
// service without one, the busiest over the last impactWindow.
func (in *Instance) measuredRoutes() []string {
	var out []string
	for _, s := range in.objectives() {
		out = append(out, s.route)
	}
	if len(out) > 0 {
		return out
	}
	rs := in.Routes(impactWindow)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Requests > rs[j].Requests })
	for _, r := range rs {
		if r.Route != unmatchedRoute && len(out) < busiest {
			out = append(out, r.Route)
		}
	}
	return out
}

// measureNow reads the default metric set over the last impactWindow.
func (in *Instance) measureNow(routes []string) map[string]float64 {
	m := map[string]float64{}
	now := time.Now() // one clock for the stats and their span, so a bucket can't rotate between
	for _, route := range routes {
		if r := in.routes.stats(route, impactWindow, now); r.Requests > 0 {
			m[route+" p99"] = msf(r.P99)
			m[route+" errors"] = float64(r.Errors) / float64(r.Requests)
			m[route+" refused"] = float64(r.Refused) / float64(r.Requests)
			m[route+" requests"] = float64(r.Requests)
			if s := in.routes.span(route, impactWindow, time.Time{}, now); s > 0 {
				m[route+" rps"] = float64(r.Requests) / s.Seconds() // windows differ in length: rates compare
			}
		}
	}
	rt := in.Runtime(impactWindow)
	m["cpu_used"], m["sched_p99"] = rt.CPUUsed, msf(rt.SchedP99)
	if rt.CPULimit > 0 {
		m["throttled"] = rt.Throttled
	}
	if rt.MemLimit > 0 {
		m["mem_used_mb"] = float64(rt.MemUsed >> 20)
	}
	m["heap_growth_mb_s"] = math.Round(rt.HeapGrowth/(1<<20)*10) / 10
	for _, s := range in.Signals() {
		if s.Err == "" {
			m[s.Name] = s.Value
		}
	}
	return m
}

// confounders names whatever else changed around a change: other changes, freezes, the environment.
func (in *Instance) confounders(change uint64, at time.Time) []Confounder {
	var out []Confounder
	seen := map[string]bool{}
	for _, e := range in.History(MaxHistory) {
		if e.At.Before(at.Add(-impactWindow)) || e.At.After(at.Add(settleFor)) {
			continue
		}
		id, _ := e.Fields["change"].(uint64)
		var what, layer string
		switch e.Kind {
		case "change":
			if id != change {
				what = fmt.Sprintf("%s changed (%v → %v) by %v", e.Fields["knob"], e.Fields["old"], e.Fields["new"], e.Fields["actor"])
				layer, _ = e.Fields["layer"].(string)
			}
		case "environment":
			what = fmt.Sprintf("the environment changed (%v %v → %v)", e.Fields["event"], e.Fields["old"], e.Fields["new"])
		case "freeze":
			what = "a freeze"
		case "unfreeze":
			what = "an unfreeze"
		}
		if what != "" && !seen[what] {
			seen[what] = true
			out = append(out, Confounder{What: what, Kind: e.Kind, Layer: layer})
		}
	}
	return out
}

// Moved is one thing a change moved, measured before and after. Every
// effect vis reports is better lower (latency, CPU, throttling), so Worse is
// whether it went up.
type Moved struct {
	Name  string  `json:"name"` // "GET /k p99", "CPU", "throttled", "sched.p99"
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	Unit  string  `json:"unit"` // "ms", "cores", "%"
	Worse bool    `json:"worse"`
}

func moved(name string, from, to float64, unit string) Moved {
	return Moved{Name: name, From: from, To: to, Unit: unit, Worse: to > from}
}

// String says it as the summaries do: "GET /k p99 243 → 61 ms", "throttled 10% → 40%".
func (e Moved) String() string {
	switch e.Unit {
	case "ms":
		return fmt.Sprintf("%s %s → %s ms", e.Name, roughMs(e.From), roughMs(e.To))
	case "%":
		return fmt.Sprintf("%s %.0f%% → %.0f%%", e.Name, e.From, e.To)
	}
	return fmt.Sprintf("%s %s → %s %s", e.Name, num(e.From), num(e.To), e.Unit)
}

// unservedMoved is how far a route's failed or refused share must move, as a
// fraction of its requests, for an impact report to say so.
const unservedMoved = .05

// effects is what moved enough to mention, at the precision it's said in, and
// the first SLO route's relative change in p99.
func (in *Instance) effects(im *Impact) ([]Moved, float64) {
	var out []Moved
	rel := math.NaN()
	slo := len(in.objectives()) > 0
	for i, route := range im.routes {
		k := route + " p99"
		b, okb := im.Before[k]
		a, oka := im.After[k]
		if okb && oka {
			out = append(out, moved(k, rough(b), rough(a), "ms"))
			if i == 0 && slo && b > 0 {
				rel = a/b - 1
			}
			// A p99 that fell because requests failed or were refused fast is no win: say how many went unserved.
			for _, u := range []struct{ key, name string }{{" errors", " failed"}, {" refused", " refused"}} {
				ub, uokb := im.Before[route+u.key]
				ua, uoka := im.After[route+u.key]
				if uokb && uoka && math.Abs(ua-ub) >= unservedMoved {
					out = append(out, moved(route+u.name, math.Round(ub*100), math.Round(ua*100), "%"))
				}
			}
		}
	}
	if b, a := im.Before["cpu_used"], im.After["cpu_used"]; b > 0 && math.Abs(a-b)/b > .1 && math.Abs(a-b) >= .05 {
		out = append(out, moved("CPU", round2(b), round2(a), "cores"))
	}
	if b, a := im.Before["throttled"], im.After["throttled"]; math.Abs(a-b) > .1 {
		out = append(out, moved("throttled", math.Round(b*100), math.Round(a*100), "%"))
	}
	if b, a := im.Before["mem_used_mb"], im.After["mem_used_mb"]; b > 0 && math.Abs(a-b) >= 16 && math.Abs(a-b)/b > .1 {
		out = append(out, moved("memory", b, a, "MiB"))
	}
	if b, a := im.Before["heap_growth_mb_s"], im.After["heap_growth_mb_s"]; math.Abs(a-b) >= .5 {
		out = append(out, moved("live heap growth", b, a, "MiB/s"))
	}
	return out, rel
}

// summarize says what moved, in a sentence.
func (in *Instance) summarize(im *Impact) string {
	s := fmt.Sprintf("After %s: ", short(im.Settle))
	switch {
	case len(im.Effects) == 0 && math.IsNaN(im.p99) && len(in.objectives()) > 0:
		s += "too few requests on " + in.objectives()[0].route + " to compare."
	case len(im.Effects) == 0:
		s += "nothing it measures moved much."
	default:
		parts := make([]string, len(im.Effects))
		for i, e := range im.Effects {
			parts[i] = e.String()
		}
		s += strings.Join(parts, ", ") + "."
	}
	s += restSummary(im)
	if im.Warming {
		s += " Read it with care: the instance had started under a minute before, and its caches and connections were still warming."
	}
	if len(im.Confounded) > 0 {
		s += " Read it with care, confounded: " + strings.Join(im.Confounded, "; ") + "."
	}
	return s
}

// restSummary says what the rest of the fleet did over the same windows, and how far that goes.
func restSummary(im *Impact) string {
	if im.Rest == nil || im.Rest.After == nil || len(im.routes) == 0 {
		return ""
	}
	key := im.routes[0] + " p99"
	rb, okb := im.Rest.Before[key]
	ra, oka := im.Rest.After[key]
	if !okb || !oka {
		return fmt.Sprintf(" The rest of the fleet (%s) had too few requests on %s to compare.", list(im.Rest.Members), im.routes[0])
	}
	who := list(im.Rest.Members)
	if len(im.Rest.Gossiped) > 0 {
		who += fmt.Sprintf("; %s from %s gossip, too busy to answer", list(im.Rest.Gossiped), plural(len(im.Rest.Gossiped), "its", "their"))
	}
	s := fmt.Sprintf(" Meanwhile the rest of the fleet (%s): %s p99 %s → %s ms.", who, im.routes[0], roughMs(rb), roughMs(ra))
	if im.Rest.AfterOf != nil {
		s += fmt.Sprintf(" After, only %s answered.", list(im.Rest.AfterOf))
	}
	switch {
	case len(im.Rest.Changed) > 0: // they were changed too: say so, rather than guess at something shared
		s += fmt.Sprintf(" The rest were changed too meanwhile (%s), so the comparison says less.", strings.Join(im.Rest.Changed, "; "))
	case rb > 0 && math.Abs(ra-rb)/rb > .25:
		s += " The rest moved too, so something they share (load, a dependency) may have moved this member as well."
	}
	return s + fmt.Sprintf(" One member against %d, one window each: a comparison, not an inference.", len(im.Rest.Members))
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// roughMs says a latency at the precision the histograms measure it: whole
// milliseconds from 10 ms up, one decimal below.
// rough is a latency at the precision the histograms measure it, as a number.
func rough(ms float64) float64 {
	if ms >= 9.95 {
		return math.Round(ms)
	}
	return math.Round(ms*10) / 10
}

func roughMs(ms float64) string {
	if ms >= 9.95 {
		return fmt.Sprint(math.Round(ms))
	}
	return num(math.Round(ms*10) / 10)
}

func unitValue(d *decl, v int64) string {
	if d == nil {
		return fmt.Sprint(v)
	}
	s := d.format(v)
	if d.unit != "" && !d.isDuration {
		s += " " + d.unit
	}
	return s
}

// observed is the best measured evidence that turning a knob helped the first
// SLO, said in a phrase: its largest unconfounded p99 improvement, or failing
// that its largest confounded one, labeled as such. The second value orders
// levers: more negative is better, and confounded evidence ranks after clean.
func (in *Instance) observed(knob string) (string, float64) {
	t := &in.impacts
	t.mu.Lock()
	defer t.mu.Unlock()
	var clean, mixed struct {
		im  *Impact
		i   int
		rel float64
	}
	for _, id := range t.order {
		im := t.byID[id]
		if !im.Done || math.IsNaN(im.p99) || im.p99 >= -.1 { // only improvements worth saying
			continue
		}
		for i, k := range im.knobs {
			best := &clean
			if len(im.Confounded) > 0 {
				best = &mixed
			}
			if k.name == knob && im.p99 < best.rel {
				best.im, best.i, best.rel = im, i, im.p99
			}
		}
	}
	say := func(im *Impact, i int) string {
		return fmt.Sprintf("last time %s: %s p99 %.0f%%", strings.TrimPrefix(im.Changes[i], knob+" "), in.objectives()[0].route, im.p99*100)
	}
	switch {
	case clean.im != nil:
		return say(clean.im, clean.i), clean.rel - 1 // any clean evidence before any confounded
	case mixed.im != nil:
		return say(mixed.im, mixed.i) + ", while other things changed too", mixed.rel
	}
	return "", 0
}

// rankLevers puts levers with measured improvements first, biggest first, then the rest by position.
func (in *Instance) rankLevers(ls []Lever) {
	gain := map[string]float64{}
	for i := range ls {
		ls[i].Observed, gain[ls[i].Knob] = in.observed(ls[i].Knob)
	}
	sort.SliceStable(ls, func(i, j int) bool {
		gi, gj := gain[ls[i].Knob], gain[ls[j].Knob]
		if gi != gj {
			return gi < gj
		}
		return ls[i].Position > ls[j].Position
	})
}

// noMeasurement is why there is no report for a change.
func (in *Instance) noMeasurement(change uint64) error {
	if err := in.formerOf("change", change); err != nil {
		return fmt.Errorf("vis: no measurement for change %d: %w", change, err)
	}
	return fmt.Errorf("vis: no measurement for change %d: it changed nothing, or is too old", change)
}
