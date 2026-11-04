package vis

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeCgroup(t *testing.T, cgroupLine, max, stat string) {
	t.Helper()
	root, proc := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "kubepods/pod1")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(max), 0o644)
	os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(stat), 0o644)
	os.WriteFile(filepath.Join(proc, "cgroup"), []byte(cgroupLine), 0o644)
	oldRoot, oldSelf := cgroupPaths()
	setCgroupPaths(root, filepath.Join(proc, "cgroup"))
	t.Cleanup(func() { setCgroupPaths(oldRoot, oldSelf) })
}

func TestCgroupReadsTheProcessesOwnLimitAndCounters(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "25000 100000\n",
		"usage_usec 5000000\nuser_usec 4000000\nsystem_usec 1000000\nnr_periods 1000\nnr_throttled 250\nthrottled_usec 2000000\n")
	c, ok := readCgroup()
	if !ok {
		t.Fatal("cgroup not read")
	}
	if c.limit != 0.25 || c.periods != 1000 || c.throttled != 250 || c.throttledTime != 2*time.Second || c.usage != 5*time.Second {
		t.Fatalf("cgroup = %+v", c)
	}
}

func TestCgroupWithoutALimit(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "max 100000\n", "usage_usec 10\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n")
	if c, ok := readCgroup(); !ok || c.limit != 0 {
		t.Fatalf("cgroup = %+v, %v; want read, with no limit", c, ok)
	}
}

func TestNoCgroupMeansNoReadings(t *testing.T) {
	oldRoot, oldSelf := cgroupPaths()
	setCgroupPaths(oldRoot, filepath.Join(t.TempDir(), "missing"))
	t.Cleanup(func() { setCgroupPaths(oldRoot, oldSelf) })
	if _, ok := readCgroup(); ok {
		t.Fatal("read a cgroup that isn't there")
	}
}

// Throttling over a window comes from the difference of two cpu.stat readings.
func TestThrottlingOverAWindow(t *testing.T) {
	older := cgroupStat{limit: .25, periods: 1000, throttled: 100, throttledTime: time.Second, usage: 10 * time.Second}
	newer := cgroupStat{limit: .25, periods: 1100, throttled: 180, throttledTime: 5 * time.Second, usage: 12500 * time.Millisecond}
	var s RuntimeStats
	s.addCgroup(&older, &newer, 10*time.Second)
	if s.CPULimit != .25 || s.Throttled != .8 || s.ThrottledTime != 4*time.Second || s.CPUUsed != .25 {
		t.Fatalf("stats = limit %v throttled %v for %v, used %v cores", s.CPULimit, s.Throttled, s.ThrottledTime, s.CPUUsed)
	}
}

// A container's CPU limit changing under the process is the kind of trigger an
// on-call agent couldn't see; vis records it in the history when it notices.
func TestACPULimitChangeIsRecorded(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "200000 100000\n", "usage_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n")
	in := NewInstance()
	in.Runtime(time.Second)
	os.WriteFile(filepath.Join(fixtureRoot(), "kubepods/pod1/cpu.max"), []byte("25000 100000\n"), 0o644)
	in.Runtime(time.Second)
	h := in.History(10)
	if len(h) != 1 || h[0].Kind != "environment" || h[0].Fields["event"] != "cpu_limit_changed" || h[0].Fields["old"] != 2.0 || h[0].Fields["new"] != 0.25 {
		t.Fatalf("history = %+v, want one cpu_limit_changed from 2 to 0.25", h)
	}
	in.Runtime(time.Second)
	if n := len(in.History(10)); n != 1 {
		t.Fatalf("history has %d events after an unchanged reading, want still 1", n)
	}
}

func TestACPULimitChangeTellsItsStory(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "200000 100000\n", "usage_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n")
	in := NewInstance()
	in.Runtime(time.Second)
	os.WriteFile(filepath.Join(fixtureRoot(), "kubepods/pod1/cpu.max"), []byte("25000 100000\n"), 0o644)
	in.Runtime(time.Second)
	s := in.Stories(10, Writer{Kind: Human, ID: "sam"})
	if len(s) != 1 || s[0].Title != "The container's CPU limit changed from 2 to 0.25 cores." || !s[0].Alert || s[0].Kind != "environment" {
		t.Fatalf("stories = %+v", s)
	}
	if !strings.Contains(s[0].Body, "Nobody changed it through vis") {
		t.Fatalf("body = %q", s[0].Body)
	}
}

func TestACPULimitStoryStopsAskingOnceTheLimitChangesAgain(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "200000 100000\n", "usage_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n")
	in := NewInstance()
	in.Runtime(time.Second)
	for _, max := range []string{"25000 100000\n", "200000 100000\n"} { // squeezed, then released
		os.WriteFile(filepath.Join(fixtureRoot(), "kubepods/pod1/cpu.max"), []byte(max), 0o644)
		in.Runtime(time.Second)
	}
	for _, s := range in.Stories(10, Writer{Kind: Human, ID: "sam"}) {
		if s.Alert {
			t.Fatalf("still asking for attention after the limit came back: %+v", s)
		}
	}
}

// Under CPU pressure vis names the pressure, with its evidence, and the knobs that spend CPU.
func TestCPUPressureNamesItsLevers(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "25000 100000\n", "usage_usec 0\nnr_periods 100\nnr_throttled 0\nthrottled_usec 0\n")
	spender := pressureSpender
	in := NewInstance()
	in.Runtime(10 * time.Second)
	if p := in.Pressures(); len(p) != 0 {
		t.Fatalf("pressure before any throttling: %+v", p)
	}
	os.WriteFile(filepath.Join(fixtureRoot(), "kubepods/pod1/cpu.stat"), []byte("usage_usec 2500000\nnr_periods 200\nnr_throttled 100\nthrottled_usec 9000000\n"), 0o644)
	p := in.Pressures()
	if len(p) != 1 || p[0].Resource != CPU {
		t.Fatalf("pressures = %+v, want CPU", p)
	}
	if !strings.Contains(p[0].Why, "throttled in 100% of periods at its 0.25-core limit") {
		t.Fatalf("why = %q", p[0].Why)
	}
	if len(p[0].Levers) == 0 || p[0].Levers[0].Knob != spender.Name() {
		t.Fatalf("levers = %+v", p[0].Levers)
	}
}

var pressureSpender = NewInt("pressure_test_compress", Range(1, 9), Default(9), Uses(CPU)) // declared once, for -count

func fixtureRoot() string { r, _ := cgroupPaths(); return r }

// Without a cgroup, as on macOS or a Linux host without cgroup v2, the CPU a
// process used still comes from the OS: an impact report there read 0 → 0
// cores, which looks like a measurement.
func TestCPUUsedWithoutACgroup(t *testing.T) {
	oldRoot, oldSelf := cgroupPaths()
	setCgroupPaths(oldRoot, filepath.Join(t.TempDir(), "missing"))
	t.Cleanup(func() { setCgroupPaths(oldRoot, oldSelf) })
	in := NewInstance()
	in.Runtime(time.Minute)
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); { // one goroutine busy: about a core
	}
	if s := in.Runtime(time.Minute); s.CPUUsed < .3 || s.CPULimit != 0 {
		t.Fatalf("CPU used = %.2f cores, limit %v; want about one core and no limit", s.CPUUsed, s.CPULimit)
	}
}

func fakeMemory(t *testing.T, current, max, stat, events string) {
	t.Helper()
	dir := filepath.Join(fixtureRoot(), "kubepods/pod1")
	for name, body := range map[string]string{"memory.current": current, "memory.max": max, "memory.stat": stat, "memory.events": events} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
}

// The container's memory, as its limit sees it: in use, less the page cache the
// kernel takes back before it kills anything (Kubernetes' working set).
func TestCgroupReadsTheContainersMemory(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "max 100000\n", "usage_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n")
	fakeMemory(t, "500000000\n", "536870912\n", "anon 400000000\nfile 100000000\ninactive_file 80000000\n", "low 0\nhigh 0\nmax 12\noom 3\noom_kill 2\n")
	c, ok := readCgroup()
	if !ok || c.memUsed != 420000000 || c.memLimit != 536870912 || c.oomKills != 2 {
		t.Fatalf("cgroup = %+v, want 420000000 of 536870912 in use and 2 OOM kills", c)
	}
	fakeMemory(t, "1000\n", "max\n", "inactive_file 0\n", "oom_kill 0\n")
	if c, _ := readCgroup(); c.memLimit != 0 || c.memUsed != 1000 {
		t.Fatalf("no memory limit: %+v, want limit 0", c)
	}
}

// Without GOMEMLIMIT, which most services leave unset, the container's limit is
// what kills the process: memory pressure must come from it, with the knobs
// that spend memory. Seen in the memory evals' case a: OOM kills with no word from vis.
func TestMemoryPressureFromTheContainersLimit(t *testing.T) {
	fakeCgroup(t, "0::/kubepods/pod1\n", "max 100000\n", "usage_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n")
	fakeMemory(t, "209715200\n", "536870912\n", "inactive_file 0\n", "oom_kill 0\n")
	spender := memorySpender
	in := NewInstance()
	if p := in.Pressures(); len(p) != 0 {
		t.Fatalf("pressure at 200 of 512 MiB: %+v", p)
	}
	fakeMemory(t, "492830720\n", "536870912\n", "inactive_file 0\n", "oom_kill 1\n")
	p := in.Pressures()
	if len(p) != 1 || p[0].Resource != Memory {
		t.Fatalf("pressures = %+v, want memory", p)
	}
	if !strings.Contains(p[0].Why, "the container uses 470 of its 512 MiB limit (92%)") {
		t.Fatalf("why = %q", p[0].Why)
	}
	if len(p[0].Levers) == 0 || p[0].Levers[0].Knob != spender.Name() {
		t.Fatalf("levers = %+v", p[0].Levers)
	}
	rt := runtimeJSON(in.Runtime(10 * time.Second))
	if rt.MemUsedMB != 470 || rt.MemLimitMB != 512 || rt.OOMKills != 1 {
		t.Fatalf("runtime = %+v, want 470 of 512 MiB and 1 OOM kill", rt)
	}
}

var memorySpender = NewInt("pressure_test_cache_mb", Range(1, 1000), Default(500), Uses(Memory)) // declared once, for -count

// Pressure says where memory is heading, not only where it is: a leak and a
// full but steady cache read the same as a level (the memory evals' case c-lim, six
// minutes of the same pressure before the kill). Growing toward the limit, it
// says when it gets there, and speaks up before 85% when that is soon.
func TestMemoryPressureSaysWhereItIsHeading(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	for _, c := range []struct {
		used, limit, growth float64 // MiB, MiB, MiB/s
		want                string  // "" for no pressure
	}{
		{200, 512, 0, ""},
		{460, 512, 0, "the container uses 460 of its 512 MiB limit (90%), steady"},
		{460, 512, 4, "the container uses 460 of its 512 MiB limit (90%), growing 4 MiB/s: at the limit in about 13 s"},
		{320, 512, 24, "the container uses 320 of its 512 MiB limit (62%), growing 24 MiB/s: at the limit in about 8 s"},
		{511.8, 512, 20, "the container uses 511 of its 512 MiB limit (100%), growing 20 MiB/s: at the limit now"},
		{320, 512, .1, ""}, // growing, but half an hour from the limit
		{100, 512, 24, ""}, // a start-up burst well below it
	} {
		rt := RuntimeStats{MemUsed: mib(c.used), MemLimit: mib(c.limit), MemGrowth: c.growth * (1 << 20)}
		if got := memoryPressure(rt, pressureAhead); got != c.want {
			t.Errorf("%v of %v MiB growing %v MiB/s: %q, want %q", c.used, c.limit, c.growth, got, c.want)
		}
	}
}

// A heap profile counts what the heap keeps, not what the container uses: in
// the memory evals an agent read "81 MiB in use" with the container at 494 of its
// 512 MiB, and concluded the cache wasn't what was growing. Without
// GOMEMLIMIT, Go lets the heap grow to about twice what it keeps before it
// collects, whatever the container's limit. So the profile says how far apart
// they are, and why.
func TestAHeapProfileSaysWhatTheContainerUsesBesides(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	unset := uint64(math.MaxInt64)
	for _, c := range []struct {
		inuse, used, limit float64 // MiB
		memLimit           uint64
		want               string
	}{
		{81, 494, 512, unset, "The container uses 494 of its 512 MiB limit, 6 times this: the rest is garbage not yet collected and memory the Go runtime holds. GOMEMLIMIT isn't set, so the collector doesn't know the limit: it lets the heap grow to about twice what it keeps before it collects."},
		{81, 494, 512, mib(460), "The container uses 494 of its 512 MiB limit, 6 times this: the rest is garbage not yet collected and memory the Go runtime holds."},
		{133, 160, 512, unset, ""}, // near what the heap keeps: nothing to explain
		{81, 494, 0, unset, ""},    // no container limit known
	} {
		rt := RuntimeStats{MemUsed: mib(c.used), MemLimit: mib(c.limit), GoMemLimit: c.memLimit}
		if got := heapBesides(rt, c.inuse); got != c.want {
			t.Errorf("%v MiB in use, the container at %v of %v: %q, want %q", c.inuse, c.used, c.limit, got, c.want)
		}
	}
}

// A container with a memory limit and a Go runtime that doesn't know it is
// killed with much of its memory garbage. vis says so once, as a warning.
func TestAContainerLimitWithoutGOMEMLIMITIsAWarning(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	if got, want := memLimitWarning(RuntimeStats{MemLimit: mib(512), GoMemLimit: math.MaxInt64}),
		"The container's memory limit is 512 MiB, but GOMEMLIMIT isn't set, so Go's collector doesn't know it: the heap grows to about twice what it keeps before a collection, and the container can be killed with much of its memory garbage. Set GOMEMLIMIT to about 90% of the limit (GOMEMLIMIT=460MiB)."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := memLimitWarning(RuntimeStats{MemLimit: mib(512), GoMemLimit: mib(460)}); got != "" {
		t.Errorf("GOMEMLIMIT set: %q", got)
	}
	if got := memLimitWarning(RuntimeStats{GoMemLimit: math.MaxInt64}); got != "" {
		t.Errorf("no container limit: %q", got)
	}
}

// A change on a process seconds from its container's memory limit is likely
// lost with it: in the memory evals an agent spent its first changes on processes
// killed within seconds. set_knob says so.
func TestAChangeOnAProcessAboutToBeKilledSaysSo(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	got := nearKill(RuntimeStats{MemUsed: mib(464), MemLimit: mib(512), MemGrowth: 25 * (1 << 20)})
	want := `This process is about 2 s from its container's memory limit (464 of 512 MiB, growing 25 MiB/s). If it is killed, the restart drops this change unless it is kept ("keep":true); the process that replaces it names it in its death story.`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, rt := range []RuntimeStats{
		{MemUsed: mib(300), MemLimit: mib(512)},                           // steady
		{MemUsed: mib(100), MemLimit: mib(512), MemGrowth: 1 * (1 << 20)}, // minutes away
		{MemUsed: mib(464), MemGrowth: 25 * (1 << 20)},                    // no limit
	} {
		if s := nearKill(rt); s != "" {
			t.Errorf("%+v: %q", rt, s)
		}
	}
}

// A slow leak hides under the garbage a busy cache churns: in the memory evals the
// leak's 0.8 MiB/s was lost in the container's sawtooth, and an agent called
// a leaking service steady. The live heap, what the GC keeps, shows it, and
// the heap the GC allows before it collects (its goal) is where the container
// peaks: pressure says when that reaches the limit.
func TestMemoryPressureSeesTheLiveHeapGrow(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	rt := RuntimeStats{MemUsed: mib(363), MemLimit: mib(512), HeapLive: mib(224), HeapGoal: mib(448), HeapGrowth: .8 * (1 << 20)}
	want := "the container uses 363 of its 512 MiB limit (71%), steady; the live heap grows 0.8 MiB/s, and the heap the GC allows before it collects, 448 MiB, reaches the limit in about 40 s"
	if got := memoryPressure(rt, pressureAhead); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	rt.HeapGrowth = 0 // the same, not leaking: no pressure
	if got := memoryPressure(rt, pressureAhead); got != "" {
		t.Errorf("a steady live heap: %q", got)
	}
}

// An impact report says what a change did to memory: agents in the memory evals
// checked memory by hand after each change.
func TestAnImpactReportSaysWhatMemoryDid(t *testing.T) {
	in := NewInstance()
	out, _ := in.effects(&Impact{Before: map[string]float64{"mem_used_mb": 480, "heap_growth_mb_s": 25}, After: map[string]float64{"mem_used_mb": 305, "heap_growth_mb_s": .1}})
	var got []string
	for _, m := range out {
		got = append(got, m.String())
	}
	s := strings.Join(got, "; ")
	for _, w := range []string{"memory 480 → 305 MiB", "live heap growth 25 → 0.1 MiB/s"} {
		if !strings.Contains(s, w) {
			t.Errorf("effects %q, want %q", s, w)
		}
	}
}

// With GOMEMLIMIT set the collector caps the heap at it, so the heap the GC
// allows doesn't grow with the live heap: what comes is the live heap reaching
// GOMEMLIMIT, where the collector runs almost without pause. Projecting the
// goal as if it grew foretold a kill in about a minute that was minutes off.
func TestMemoryPressureWithGOMEMLIMITForetellsTheLiveHeapReachingIt(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	rt := RuntimeStats{MemUsed: mib(300), MemLimit: mib(512), GoMemLimit: mib(460), HeapLive: mib(200), HeapGoal: mib(440), HeapGrowth: .5 * (1 << 20)}
	if got := memoryPressure(rt, pressureAhead); got != "" {
		t.Errorf("about 9 minutes from GOMEMLIMIT: %q", got)
	}
	rt.HeapGrowth = 3 * (1 << 20)
	want := "the container uses 300 of its 512 MiB limit (59%), steady; the live heap grows 3 MiB/s and reaches GOMEMLIMIT, 460 MiB, in about 87 s: then the collector runs almost without pause, and the container is killed soon after"
	if got := memoryPressure(rt, pressureAhead); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A change made seconds before the heap the GC allows reaches the limit is
// as likely lost as one made seconds before the container's memory does: the
// container peaks at that heap at its next collection.
func TestAChangeJustBeforeTheHeapReachesTheLimitSaysSo(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	rt := RuntimeStats{MemUsed: mib(363), MemLimit: mib(512), HeapLive: mib(224), HeapGoal: mib(448), HeapGrowth: 8 * (1 << 20)}
	want := `This process is about 4 s from its container's memory limit (its live heap grows 8 MiB/s, and the heap the GC allows before it collects, 448 MiB, with it). If it is killed, the restart drops this change unless it is kept ("keep":true); the process that replaces it names it in its death story.`
	if got := nearKill(rt); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A member whose last word foretold the limit within seconds most likely died
// for memory, though its container stood well below it: the leak's kills in
// the memory evals came at the GC's peak, between heartbeats.
func TestADeathForetoldByTheHeapIsForMemory(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	if !killedForMemory(&memDigest{Used: mib(363), Limit: mib(512), Kill: 3 * time.Second}) {
		t.Error("a kill foretold in 3 s: not read as for memory")
	}
	if killedForMemory(&memDigest{Used: mib(363), Limit: mib(512), Kill: 5 * time.Minute}) {
		t.Error("a kill foretold in 5 min: read as for memory")
	}
}

// Pressure looks two minutes ahead, so with the cache lowered under a leak
// vis said nothing of the next kill, and an agent guessed it 3 to 8 minutes
// off when it came in 2 (the memory evals, keep build). The memory outlook says
// when the limit comes at the current rate, however far, or that it doesn't
// within the hour.
func TestTheMemoryOutlookSaysWhenTheLimitComes(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	for _, c := range []struct {
		rt   RuntimeStats
		want string
	}{
		{RuntimeStats{MemUsed: mib(310), MemLimit: mib(512), HeapLive: mib(150), HeapGoal: mib(300), HeapGrowth: .5 * (1 << 20)},
			"At the rate of the last 45 s, the container reaches its 512 MiB memory limit in about 210 s: it uses 310 MiB; the live heap grows 0.5 MiB/s, and the heap the GC allows before it collects, 300 MiB, with it."},
		{RuntimeStats{MemUsed: mib(460), MemLimit: mib(512), MemGrowth: 4 * (1 << 20)},
			"At the rate of the last 45 s, the container reaches its 512 MiB memory limit in about 13 s: it uses 460 MiB, growing 4 MiB/s."},
		{RuntimeStats{MemUsed: mib(300), MemLimit: mib(512), HeapLive: mib(150), HeapGoal: mib(300)},
			"At the rate of the last 45 s, the container doesn't reach its 512 MiB memory limit within the hour: it uses 300 MiB."},
		{RuntimeStats{MemUsed: mib(300)}, ""}, // no limit known
	} {
		if got := memoryOutlook(c.rt, 45*time.Second); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}

// The outlook is measured since the last change, so the cache a change
// shrank doesn't hide the growth after it: an agent read -0.81 MiB/s over
// 30 s just after lowering the cache and thought the leak had stopped.
func TestTheMemoryOutlookIsMeasuredSinceTheLastChange(t *testing.T) {
	for _, c := range []struct{ since, want time.Duration }{
		{5 * time.Second, outlookMin}, {30 * time.Second, 30 * time.Second}, {10 * time.Minute, outlookMax}, {-1, outlookMax},
	} {
		if got := outlookWindow(c.since); got != c.want {
			t.Errorf("since the last change %v: window %v, want %v", c.since, got, c.want)
		}
	}
}

// Rounded to the whole minute, the outlook's "about 2 min" stood for a kill
// that came 170 s on, and the agent took it for 120 (the memory evals, outlook
// build). Under 5 minutes a time is said in seconds.
func TestTimesUnderFiveMinutesAreSaidInSeconds(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{13 * time.Second, "13 s"},
		{119 * time.Second, "119 s"},
		{170 * time.Second, "170 s"},
		{212 * time.Second, "210 s"},
		{296 * time.Second, "300 s"},
		{5 * time.Minute, "5 min"},
		{9*time.Minute + 20*time.Second, "9 min"},
	} {
		if got := about(c.d); got != c.want {
			t.Errorf("about(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
