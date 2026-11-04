package vis

import (
	"log/slog"
	"math"
	"os"
	"runtime/metrics"
	"strconv"
	"sync"
	"time"
)

// RuntimeStats is probe tier 1: the Go runtime's own counters, nearly free to
// read. Rates and percentiles cover a window, measured as the difference
// between two samples of cumulative counters.
type RuntimeStats struct {
	Over       time.Duration // the span the window actually covers; shorter until enough history exists
	SchedP50   time.Duration // how long runnable goroutines waited to run: CPU pressure shows here first
	SchedP99   time.Duration
	GCCPU      float64 // share of the process's CPU time spent on garbage collection
	HeapGoal   uint64  // the heap size the GC is aiming for
	GoMemLimit uint64  // GOMEMLIMIT; math.MaxInt64 when unset
	HeapRatio  float64 // HeapGoal / MemLimit, 0 with no limit; an OOM kill comes after 1
	Goroutines int
	GOMAXPROCS int

	// Container signals, from the process's cgroup (Linux, cgroup v2). Zero elsewhere.
	CPULimit      float64       // cores the container may use; 0 when unlimited or unknown
	CPUUsed       float64       // cores used over the window: the cgroup's, or without one the process's own, from the OS (any Unix)
	Throttled     float64       // share of scheduling periods in which the container hit its limit and was paused
	ThrottledTime time.Duration // how long it was paused over the window
	// Memory: what the process holds against where it ends. In a cgroup (a
	// container, a Kubernetes pod, a systemd unit with MemoryMax) the cgroup's
	// working set, less the page cache the kernel reclaims first; elsewhere (a
	// VM, a Mac, Lambda) what the Go runtime holds.
	MemFrom  string // "container" (a cgroup) or "process"
	MemUsed  uint64 // bytes in use
	MemLimit uint64 // bytes: the cgroup's memory.max, else Lambda's memory size, else GOMEMLIMIT; 0 when none
	// MemLimitBy is what sets MemLimit: "cgroup" or "lambda", where the
	// process is killed, or "GOMEMLIMIT", where the collector runs almost
	// without pause; "" when there is no limit.
	MemLimitBy string
	OOMKills   uint64  // processes the kernel killed in this container for memory, since it began
	MemGrowth  float64 // how fast MemUsed grew over the window, bytes a second; negative as it shrinks

	// The live heap: what the GC kept at its last cycle. Unlike the container's
	// memory it has no garbage in it, so a slow leak under a churning cache shows here.
	HeapLive   uint64
	HeapGrowth float64 // how fast it grew over the window, bytes a second
}

const (
	mSched      = "/sched/latencies:seconds"
	mGCCPU      = "/cpu/classes/gc/total:cpu-seconds"
	mTotalCPU   = "/cpu/classes/total:cpu-seconds"
	mHeapGoal   = "/gc/heap/goal:bytes"
	mMemLimit   = "/gc/gomemlimit:bytes"
	mHeapLive   = "/gc/heap/live:bytes"
	mGoroutines = "/sched/goroutines:goroutines"
	mProcs      = "/sched/gomaxprocs:threads"
	mMapped     = "/memory/classes/total:bytes"
	mReleased   = "/memory/classes/heap/released:bytes"
)

type rtSample struct {
	at              time.Time
	sched           *metrics.Float64Histogram
	gcCPU, totalCPU float64
	heapLive        uint64
	goHeld          uint64        // what the Go runtime holds: all it mapped, less the heap it released to the OS
	cg              *cgroupStat   // nil without a cgroup
	proc            time.Duration // CPU the process used, from the OS; for CPU used where there is no cgroup
	procOK          bool
}

// runtimeRing keeps recent samples, taken whenever someone asks, so windows
// work without a goroutine ticking in the background.
type runtimeRing struct {
	mu      sync.Mutex
	samples []rtSample // oldest first

	envMu     sync.Mutex // the last environment seen, for noticing changes
	envSeen   bool
	lastLimit float64
	lastProcs int
}

const (
	rtEvery = 500 * time.Millisecond // keep at most one sample per this
	rtKeep  = 1200                   // ten minutes of history at that rate
)

// Runtime reads the runtime's counters now and summarizes the last window.
func (in *Instance) Runtime(window time.Duration) RuntimeStats {
	ms := []metrics.Sample{{Name: mSched}, {Name: mGCCPU}, {Name: mTotalCPU}, {Name: mHeapGoal},
		{Name: mMemLimit}, {Name: mGoroutines}, {Name: mProcs}, {Name: mHeapLive}, {Name: mMapped}, {Name: mReleased}}
	metrics.Read(ms)
	now := time.Now()
	cur := rtSample{at: now, sched: ms[0].Value.Float64Histogram(), gcCPU: ms[1].Value.Float64(), totalCPU: ms[2].Value.Float64(), heapLive: ms[7].Value.Uint64(),
		goHeld: ms[8].Value.Uint64() - min(ms[9].Value.Uint64(), ms[8].Value.Uint64())}
	if c, ok := readCgroup(); ok {
		cur.cg = &c
	}
	cur.proc, cur.procOK = procCPU()
	in.noticeEnvironment(cur.cg, int(ms[6].Value.Uint64()))
	s := RuntimeStats{HeapGoal: ms[3].Value.Uint64(), GoMemLimit: ms[4].Value.Uint64(),
		Goroutines: int(ms[5].Value.Uint64()), GOMAXPROCS: int(ms[6].Value.Uint64()), HeapLive: cur.heapLive}
	if s.GoMemLimit > 0 && s.GoMemLimit < math.MaxInt64 {
		s.HeapRatio = float64(s.HeapGoal) / float64(s.GoMemLimit)
	}

	r := &in.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if base := r.base(now.Add(-window)); base != nil {
		s.Over = now.Sub(base.at)
		s.SchedP50 = windowQuantile(base.sched, cur.sched, .50)
		s.SchedP99 = windowQuantile(base.sched, cur.sched, .99)
		if total := cur.totalCPU - base.totalCPU; total > 0 {
			s.GCCPU = (cur.gcCPU - base.gcCPU) / total
		}
		if s.Over > 0 && base.heapLive > 0 {
			s.HeapGrowth = (float64(cur.heapLive) - float64(base.heapLive)) / s.Over.Seconds()
		}
		if s.Over > 0 && base.memUsed() > 0 {
			s.MemGrowth = (float64(cur.memUsed()) - float64(base.memUsed())) / s.Over.Seconds()
		}
		if base.cg != nil && cur.cg != nil {
			s.addCgroup(base.cg, cur.cg, s.Over)
		} else if base.procOK && cur.procOK && s.Over > 0 { // no cgroup: the process's own CPU time, so CPU used isn't a 0 that looks measured
			s.CPUUsed = float64(cur.proc-base.proc) / float64(s.Over)
		}
	} else if cur.cg != nil {
		s.CPULimit = cur.cg.limit
	}
	if cur.cg != nil {
		s.addMemory(cur.cg) // a level: the reading now, with or without a window
	}
	s.MemUsed, s.MemFrom = cur.memUsed(), "container"
	if cur.cg == nil || cur.cg.memUsed == 0 {
		s.MemFrom = "process"
	}
	switch l := lambdaMemory(); {
	case s.MemLimit > 0:
		s.MemLimitBy = "cgroup"
	case l > 0:
		s.MemLimit, s.MemLimitBy = l, "lambda"
	case gcKnowsLimit(s):
		s.MemLimit, s.MemLimitBy = s.GoMemLimit, "GOMEMLIMIT"
	}
	if n := len(r.samples); n == 0 || now.Sub(r.samples[n-1].at) >= rtEvery {
		r.samples = append(r.samples, cur)
		if len(r.samples) > rtKeep {
			r.samples = r.samples[1:]
		}
	}
	return s
}

// base is the newest sample at or before from, or the oldest one when history is shorter than the window.
func (r *runtimeRing) base(from time.Time) *rtSample {
	if len(r.samples) == 0 {
		return nil
	}
	for i := len(r.samples) - 1; i >= 0; i-- {
		if !r.samples[i].at.After(from) {
			return &r.samples[i]
		}
	}
	return &r.samples[0]
}

// windowQuantile is a percentile of what was added to a cumulative histogram
// between two samples. It reports a bucket's middle, the lower edge of the
// open top bucket, and the upper edge of the open bottom one.
func windowQuantile(older, newer *metrics.Float64Histogram, q float64) time.Duration {
	var total uint64
	diff := make([]uint64, len(newer.Counts))
	for i, c := range newer.Counts {
		if i < len(older.Counts) {
			c -= older.Counts[i]
		}
		diff[i] = c
		total += c
	}
	if total == 0 {
		return 0
	}
	rank := uint64(q*float64(total-1)) + 1
	var seen uint64
	for i, c := range diff {
		seen += c
		if seen < rank {
			continue
		}
		lo, hi := newer.Buckets[i], newer.Buckets[i+1]
		var sec float64
		switch {
		case math.IsInf(hi, 1):
			sec = lo
		case math.IsInf(lo, -1):
			sec = hi
		default:
			sec = (lo + hi) / 2
		}
		return time.Duration(math.Round(max(sec, 0) * 1e9))
	}
	return 0
}

// noticeEnvironment records changes vis can see but nobody made through it:
// the container's CPU limit and GOMAXPROCS. An agent asking "what changed?"
// finds them in the history next to knob changes.
func (in *Instance) noticeEnvironment(cg *cgroupStat, procs int) {
	r := &in.runtime
	r.envMu.Lock()
	defer r.envMu.Unlock()
	var limit float64
	if cg != nil {
		limit = cg.limit
	}
	if r.envSeen && limit != r.lastLimit {
		in.audit(slog.LevelWarn, "vis.environment", slog.String("event", "cpu_limit_changed"), slog.Float64("old", r.lastLimit), slog.Float64("new", limit))
	}
	if r.envSeen && procs != r.lastProcs {
		in.audit(slog.LevelInfo, "vis.environment", slog.String("event", "gomaxprocs_changed"), slog.Int("old", r.lastProcs), slog.Int("new", procs))
	}
	r.envSeen, r.lastLimit, r.lastProcs = true, limit, procs
}

// memPoint is the container's memory and the live heap at a moment, for the
// page's memory figures.
type memPoint struct {
	At     time.Time `json:"at"`
	UsedMB float64   `json:"used_mb"`
	HeapMB float64   `json:"heap_mb"` // the live heap, as the GC last measured it
}

// memHistory is the container's memory over the last 10 minutes of the
// samples kept, the last sample of each 10 s, oldest first; nil outside a
// container with a memory reading.
func (in *Instance) memHistory() []memPoint {
	r := &in.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []memPoint
	from := time.Now().Add(-10 * time.Minute)
	for _, s := range r.samples {
		if s.memUsed() == 0 || s.at.Before(from) {
			continue
		}
		p := memPoint{At: s.at, UsedMB: math.Round(float64(s.memUsed())/(1<<20)*10) / 10, HeapMB: math.Round(float64(s.heapLive)/(1<<20)*10) / 10}
		if n := len(out); n > 0 && s.at.Truncate(10*time.Second).Equal(out[n-1].At.Truncate(10*time.Second)) {
			out[n-1] = p // the bucket's last
			continue
		}
		out = append(out, p)
	}
	return out
}

// memUsed is the sample's memory in use: its cgroup's working set, or
// without one what the Go runtime holds.
func (s *rtSample) memUsed() uint64 {
	if s.cg != nil && s.cg.memUsed > 0 {
		return s.cg.memUsed
	}
	return s.goHeld
}

// lambdaMemory is an AWS Lambda function's memory size, where it is killed;
// 0 off Lambda.
func lambdaMemory() uint64 {
	mb, err := strconv.ParseUint(os.Getenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE"), 10, 64)
	if err != nil {
		return 0
	}
	return mb << 20
}

// memKills is whether reaching MemLimit kills the process (a cgroup's limit,
// Lambda's memory size), not only makes the collector work (GOMEMLIMIT). A
// limit given with no source is a cgroup's, as it was before the others.
func (rt RuntimeStats) memKills() bool { return rt.MemLimit > 0 && rt.MemLimitBy != "GOMEMLIMIT" }

// memWho is what holds the memory, in words.
func (rt RuntimeStats) memWho() string {
	if rt.MemFrom == "process" {
		return "the process"
	}
	return "the container"
}

// limitShort and limitLong name the limit, as "its 512 MiB limit" and "its
// 512 MiB memory limit" say it.
func (rt RuntimeStats) limitShort() string {
	switch rt.MemLimitBy {
	case "lambda":
		return "Lambda memory size"
	case "GOMEMLIMIT":
		return "GOMEMLIMIT"
	}
	return "limit"
}

func (rt RuntimeStats) limitLong() string {
	if rt.MemLimitBy == "cgroup" || rt.MemLimitBy == "" {
		return "memory limit"
	}
	return rt.limitShort()
}
