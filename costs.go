package vis

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Resource is something turning a knob up uses or saves: CPU or Memory, which
// vis measures itself, or one of the app's own gauges, whose meaning is the
// app's, named by the gauge.
type Resource string

const (
	CPU    Resource = "cpu"    // the process's CPU time, against the container's limit
	Memory Resource = "memory" // heap, against the memory limit
)

// Cost is what Uses and Saves take: CPU, Memory, or a gauge.
type Cost interface{ resource() Resource }

func (r Resource) resource() Resource   { return r }
func (g *Gauge) resource() Resource     { return Resource(g.name) }
func (l *Latency) resource() Resource   { return Resource(l.g.name) }
func (g *GaugeFunc) resource() Resource { return Resource(g.name) }

// Uses says that turning the knob up uses more of each cost: a bigger cache
// uses more memory. When the service runs short of CPU or memory, the UI and
// agents are told which knobs use it, and how far up each one is, so whoever
// is on call knows what to turn down.
func Uses(cs ...Cost) Option {
	return func(d *decl) {
		for _, c := range cs {
			d.spends = append(d.spends, spend{c, false})
		}
	}
}

// Saves says that turning the knob up saves each cost, so turning it down
// spends more: a bigger cache saves origin fetches, a higher log level saves
// the CPU that logging takes.
func Saves(cs ...Cost) Option {
	return func(d *decl) {
		for _, c := range cs {
			d.spends = append(d.spends, spend{c, true})
		}
	}
}

type spend struct {
	c     Cost // read when needed: a gauge vis gen declares is named in its init, after the knob
	lower bool // a cost it saves: lower values spend more
}

func (s spend) r() Resource { return s.c.resource() }

// Lever is a knob that spends a resource, and how expensive its current setting is.
type Lever struct {
	Knob     string  `json:"knob"`
	Value    string  `json:"value"`    // as people read it, with its unit
	Position float64 `json:"position"` // 0 the cheapest setting, 1 the most expensive
	About    string  `json:"about,omitempty"`
	Observed string  `json:"observed,omitempty"`    // what turning it did before, measured: "last time 9 → 6: GET /k p99 -97%"
	People   bool    `json:"people_only,omitempty"` // agents may not pull it (Pull doesn't list them): an agent can only ask a person to
}

// Levers lists the knobs whose setting costs r, the most expensive setting first.
func (in *Instance) Levers(r Resource) []Lever {
	cfg := in.Snapshot()
	var out []Lever
	for _, d := range allDecls() {
		for _, s := range d.spends {
			if s.r() != r {
				continue
			}
			v := cfg.get(d)
			pos := position(d, v)
			if s.lower {
				pos = 1 - pos
			}
			val := d.format(v)
			if d.unit != "" {
				val += " " + d.unit
			}
			out = append(out, Lever{Knob: d.name, Value: val, Position: math.Round(pos*100) / 100, About: d.about, People: d.pull&(1<<Agent) == 0})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position > out[j].Position })
	in.rankLevers(out) // measured evidence first; position only for knobs never changed
	return out
}

// position is how far v sits in the knob's range, 0 to 1, on a log scale for wide ranges.
func position(d *decl, v int64) float64 {
	if d.max == d.min || d.isSpecial(v) { // a special value isn't on the scale: vis doesn't know what it costs
		return 0
	}
	if d.min > 0 && d.max/d.min >= 100 {
		return math.Log(float64(v)/float64(d.min)) / math.Log(float64(d.max)/float64(d.min))
	}
	return float64(v-d.min) / float64(d.max-d.min)
}

// memForecast is where a process's memory is heading against its container's
// limit: the one projection memory pressure, a change's warning and a death's
// cause read. Two things reach the limit. The container's memory, at its
// growth. And the heap: without GOMEMLIMIT the collector lets the heap grow to
// its goal, about twice the live heap, before it collects, so the container
// peaks there and the goal grows with the live heap; a leak under the garbage
// of a churning cache shows here first. With GOMEMLIMIT the goal is capped,
// and what comes is the live heap reaching it, where the collector runs
// almost without pause.
type memForecast struct {
	rt     RuntimeStats
	frac   float64       // the container's memory, of its limit
	box    time.Duration // until the container's memory reaches the limit; -1 when it isn't growing toward it
	heap   time.Duration // until the heap does (the goal, or the live heap GOMEMLIMIT); -1 when the live heap isn't growing
	capped bool          // GOMEMLIMIT is set: the heap's end is the collector thrashing at it
}

func forecastMemory(rt RuntimeStats) memForecast {
	f := memForecast{rt: rt, box: -1, heap: -1}
	if rt.MemLimit == 0 {
		return f
	}
	limit := float64(rt.MemLimit)
	f.frac = float64(rt.MemUsed) / limit
	if rt.MemGrowth > 0 {
		f.box = seconds(max(limit-float64(rt.MemUsed), 0) / rt.MemGrowth)
	}
	if rt.HeapGrowth < .05*(1<<20) || rt.HeapLive == 0 || rt.HeapGoal == 0 {
		return f
	}
	if f.capped = gcKnowsLimit(rt); f.capped {
		f.heap = seconds(max(min(float64(rt.GoMemLimit), limit)-float64(rt.HeapLive), 0) / rt.HeapGrowth)
	} else {
		g := rt.HeapGrowth * float64(rt.HeapGoal) / float64(rt.HeapLive)
		f.heap = seconds(max(limit-float64(rt.HeapGoal), 0) / g)
	}
	return f
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// gcKnowsLimit is whether GOMEMLIMIT is set (MemLimit is MaxInt64 when not).
func gcKnowsLimit(rt RuntimeStats) bool { return rt.GoMemLimit > 0 && rt.GoMemLimit < math.MaxInt64 }

// kill is the sooner of the two ends; -1 when neither is coming.
func (f memForecast) kill() time.Duration {
	switch {
	case f.box < 0:
		return f.heap
	case f.heap < 0:
		return f.box
	}
	return min(f.box, f.heap)
}

// limitIn is how soon memory reaches the limit at its current rate, for the
// page's memory figure; -1 when it isn't heading there, or while the trend
// isn't trusted (trendTrusted), as pressure reads it.
func limitIn(rt RuntimeStats, trusted bool) time.Duration {
	if !trusted || rt.MemLimit == 0 {
		return -1
	}
	return forecastMemory(rt).kill()
}

// memoryPressure says how near the container's memory limit the process is
// and where it is heading, or "" when it isn't near: over 85% of the limit, or
// over half of it and reaching it within two minutes. A leak and a full but
// steady cache read alike as a level; the growth tells them apart.
// trendTrusted says whether memory's growth foretells a kill yet. While a
// process starts, a cache filling to its size grows as fast as a leak, and an
// alarm on it is false; unless the former process was killed for memory while
// starting too, as a cache that doesn't fit kills each process 20 to 30 s in.
func trendTrusted(started, now time.Time, former *formerProcess) bool {
	return now.Sub(started) >= warmUp || former != nil && former.forMemory && former.up < warmUp
}

// How far ahead memory's trend raises pressure, and holds it once raised:
// a forecast hovering at the edge would otherwise switch it every few seconds.
const (
	pressureAhead = 2 * time.Minute
	pressureHold  = 3 * time.Minute
)

// memoryPressure is the container short of memory: at 85% of its limit, or
// its forecast reaching the limit within ahead (0 while the trend isn't
// trusted, trendTrusted).
func memoryPressure(rt RuntimeStats, ahead time.Duration) string {
	f := forecastMemory(rt)
	if rt.MemLimit == 0 {
		return ""
	}
	soon := f.box >= 0 && f.box <= ahead && f.frac >= .5
	heapSoon := f.heap >= 0 && f.heap <= ahead && float64(rt.HeapGoal) >= .5*float64(rt.MemLimit)
	if f.frac < .85 && !soon && !heapSoon {
		return ""
	}
	s := fmt.Sprintf("%s uses %d of its %d MiB %s (%.0f%%)", rt.memWho(), rt.MemUsed>>20, rt.MemLimit>>20, rt.limitShort(), f.frac*100)
	if rt.MemGrowth >= .05*(1<<20) && f.box <= time.Hour {
		at := "at the limit in about " + about(f.box)
		if f.box < time.Second {
			at = "at the limit now"
		}
		s += fmt.Sprintf(", growing %s MiB/s: %s", mibs(rt.MemGrowth), at)
	} else {
		s += ", steady"
	}
	if f.heap >= 0 && f.heap <= time.Hour {
		s += "; " + f.heapSays()
	}
	return s
}

// heapWhy is why the heap reaches the limit, in words.
func (f memForecast) heapWhy() string {
	if f.capped {
		return fmt.Sprintf("live heap grows %s MiB/s into GOMEMLIMIT, %d MiB", mibs(f.rt.HeapGrowth), min(f.rt.GoMemLimit, f.rt.MemLimit)>>20)
	}
	return fmt.Sprintf("live heap grows %s MiB/s, and the heap the GC allows before it collects, %d MiB, with it", mibs(f.rt.HeapGrowth), f.rt.HeapGoal>>20)
}

// The memory outlook looks as far ahead as the forecast reaches, unlike
// pressure's two minutes, over the time since the last change: between
// outlookMin, for a rate worth reading, and outlookMax.
const (
	outlookMin = 10 * time.Second
	outlookMax = time.Minute
)

// outlookWindow is the window the outlook reads, since the last change (-1: none yet).
func outlookWindow(since time.Duration) time.Duration {
	if since < 0 {
		return outlookMax
	}
	return min(max(since, outlookMin), outlookMax)
}

// memoryOutlook says when the container reaches its memory limit at the rate
// of the last over, or that it doesn't within the hour; "" with no limit known.
func memoryOutlook(rt RuntimeStats, over time.Duration) string {
	if rt.MemLimit == 0 {
		return ""
	}
	f := forecastMemory(rt)
	head := fmt.Sprintf("At the rate of the last %s, %s ", about(over), rt.memWho())
	used, limit := rt.MemUsed>>20, rt.MemLimit>>20
	k := f.kill()
	if k < 0 || k > time.Hour {
		return head + fmt.Sprintf("doesn't reach its %d MiB %s within the hour: it uses %d MiB.", limit, rt.limitLong(), used)
	}
	at := "in about " + about(k)
	if k < time.Second {
		at = "now"
	}
	why := fmt.Sprintf("it uses %d MiB, growing %s MiB/s", used, mibs(rt.MemGrowth))
	if k != f.box {
		why = fmt.Sprintf("it uses %d MiB; the %s", used, f.heapWhy())
	}
	return head + fmt.Sprintf("reaches its %d MiB %s %s: %s.", limit, rt.limitLong(), at, why)
}

// memoryOutlook is the instance's, measured since its last change.
func (in *Instance) memoryOutlook() string {
	in.mu.Lock()
	since := time.Duration(-1)
	if !in.changedAt.IsZero() {
		since = time.Since(in.changedAt)
	}
	in.mu.Unlock()
	rt := in.Runtime(outlookWindow(since))
	if rt.Over < outlookMin/2 { // too little history for a rate
		return ""
	}
	return memoryOutlook(rt, rt.Over.Round(time.Second))
}

// heapSays is the heap's projection in words.
func (f memForecast) heapSays() string {
	if f.capped {
		at := "in about " + about(f.heap)
		if f.heap < time.Second {
			at = "now"
		}
		then := ""
		if f.rt.memKills() {
			then = ", and " + f.rt.memWho() + " is killed soon after"
		}
		return fmt.Sprintf("the live heap grows %s MiB/s and reaches GOMEMLIMIT, %d MiB, %s: then the collector runs almost without pause%s",
			mibs(f.rt.HeapGrowth), min(f.rt.GoMemLimit, f.rt.MemLimit)>>20, at, then)
	}
	at := "reaches the limit in about " + about(f.heap)
	if f.heap < time.Second {
		at = "is at the limit"
	}
	return fmt.Sprintf("the live heap grows %s MiB/s, and the heap the GC allows before it collects, %d MiB, %s", mibs(f.rt.HeapGrowth), f.rt.HeapGoal>>20, at)
}

// mibs is bytes a second as MiB a second, to a tenth.
func mibs(b float64) string { return num(math.Round(b/(1<<20)*10) / 10) }

// about is a time to a limit as vis says it: seconds under 2 minutes, to the
// 10 s under 5, then whole minutes.
func about(d time.Duration) string {
	switch {
	case d >= 5*time.Minute:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
	case d >= 2*time.Minute:
		return fmt.Sprintf("%d s", int(d.Round(10*time.Second)/time.Second))
	}
	return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
}

// heapBesides says, for a heap profile of inuse MiB, what the container uses
// besides, when that is far more: a heap profile counts what the heap keeps,
// and an agent reading one in the memory evals took the gap for something else
// growing. "" when they are near or no container limit is known.
func heapBesides(rt RuntimeStats, inuse float64) string {
	used := float64(rt.MemUsed) / (1 << 20)
	if rt.MemLimit == 0 || inuse <= 0 || used < 1.5*inuse {
		return ""
	}
	s := fmt.Sprintf("The container uses %d of its %d MiB limit, %.0f times this: the rest is garbage not yet collected and memory the Go runtime holds.",
		rt.MemUsed>>20, rt.MemLimit>>20, used/inuse)
	if !gcKnowsLimit(rt) {
		s += " GOMEMLIMIT isn't set, so the collector doesn't know the limit: it lets the heap grow to about twice what it keeps before it collects."
	}
	return s
}

// nearKill is the warning for a change on a process reaching its container's
// memory limit within 15 s: a kill would take the change with it.
func nearKill(rt RuntimeStats) string {
	f := forecastMemory(rt)
	eta := f.kill()
	if eta < 0 || eta > 15*time.Second || !rt.memKills() {
		return ""
	}
	why := fmt.Sprintf("%d of %d MiB, growing %s MiB/s", rt.MemUsed>>20, rt.MemLimit>>20, mibs(rt.MemGrowth))
	if eta != f.box {
		why = "its " + f.heapWhy()
	}
	where := "container's memory limit"
	if rt.MemLimitBy == "lambda" {
		where = "Lambda memory size"
	}
	return fmt.Sprintf("This process is about %d s from its %s (%s). If it is killed, the restart drops this change unless it is kept (\"keep\":true); the process that replaces it names it in its death story.",
		int(eta.Round(time.Second)/time.Second), where, why)
}

// memLimitWarning is the warning for a container with a memory limit whose Go
// runtime doesn't know it: the heap grows to about twice what it keeps
// (GOGC=100) before a collection, so the kernel kills a process whose memory
// is largely garbage. "" when GOMEMLIMIT is set or there is no limit.
func memLimitWarning(rt RuntimeStats) string {
	if !rt.memKills() || gcKnowsLimit(rt) {
		return ""
	}
	whose := "The container's memory limit"
	if rt.MemLimitBy == "lambda" {
		whose = "The function's Lambda memory size"
	}
	return fmt.Sprintf("%s is %d MiB, but GOMEMLIMIT isn't set, so Go's collector doesn't know it: the heap grows to about twice what it keeps before a collection, and %s can be killed with much of its memory garbage. Set GOMEMLIMIT to about 90%% of the limit (GOMEMLIMIT=%dMiB).",
		whose, rt.MemLimit>>20, rt.memWho(), rt.MemLimit*9/10>>20)
}

// Pressure is a resource running short now, why vis thinks so, and the knobs that spend it.
type Pressure struct {
	Resource Resource `json:"resource"`
	Why      string   `json:"why"`
	Levers   []Lever  `json:"levers"`
}

// Pressures lists the resources running short over the last 10 seconds: CPU when
// the container is throttled or goroutines wait long for a CPU, memory when the
// container nears its memory limit or the heap nears GOMEMLIMIT. Each comes with the knobs that spend it.
func (in *Instance) Pressures() []Pressure {
	rt := in.Runtime(10 * time.Second)
	var out []Pressure
	switch {
	case rt.CPULimit > 0 && rt.Throttled > .25:
		out = append(out, Pressure{Resource: CPU, Why: fmt.Sprintf("throttled in %.0f%% of periods at its %s-core limit", rt.Throttled*100, num(rt.CPULimit))})
	case rt.SchedP99 > 20*time.Millisecond:
		out = append(out, Pressure{Resource: CPU, Why: fmt.Sprintf("goroutines wait %s for a CPU at p99", rt.SchedP99.Round(time.Millisecond))})
	}
	ahead := time.Duration(0)
	if trendTrusted(in.started, time.Now(), in.former.Load()) {
		ahead = pressureAhead
		if in.memShort.Load() {
			ahead = pressureHold
		}
	}
	why := memoryPressure(rt, ahead)
	in.memShort.Store(why != "")
	switch { // the container's limit is what kills the process, GOMEMLIMIT or not
	case why != "":
		out = append(out, Pressure{Resource: Memory, Why: why})
	case rt.HeapRatio > .85:
		out = append(out, Pressure{Resource: Memory, Why: fmt.Sprintf("the heap goal is at %.0f%% of the memory limit", rt.HeapRatio*100)})
	}
	for i := range out {
		out[i].Levers = in.Levers(out[i].Resource)
	}
	return out
}
