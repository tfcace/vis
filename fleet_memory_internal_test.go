package vis

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// On call for a fleet under memory load, b was 75 s from its limit while the
// rest weren't, yet only c, in a crash loop, stood out: running short of
// memory where the rest aren't is a reason on its own.
func TestAMemberShortOfMemoryWhereTheRestArentStandsOut(t *testing.T) {
	ok := []MemberSLO{{Route: "GET /k", P99: 20 * time.Millisecond, Requests: 500, Met: true}}
	short := []Pressure{{Resource: Memory, Why: "the container uses 444 of its 512 MiB limit (87%), growing 0.9 MiB/s: at the limit in about 75 s"}}
	ms := []FleetMember{
		{Member: Member{Name: "a", Self: true}, SLOs: ok},
		{Member: Member{Name: "b"}, SLOs: ok, Pressures: short},
		{Member: Member{Name: "c"}, SLOs: ok},
	}
	var why string
	for _, s := range standOut(ms, nil, 2*time.Second) {
		if s.Member == "b" {
			why = s.Why
		}
	}
	if !strings.Contains(why, "short of memory") || !strings.Contains(why, "75 s") {
		t.Errorf("b's reason = %q, want it short of memory, at the limit in about 75 s", why)
	}
	// b's leak: the same build and knobs as the rest, who hold steady. No setting explains it.
	if !strings.Contains(why, "nothing vis compares differs") || !strings.Contains(why, `profile with instance="b"`) {
		t.Errorf("b's reason = %q, want that no setting explains it, and the profile to take", why)
	}

	ms[0].Pressures, ms[2].Pressures = short, short // every member short: what they share, not a standout
	for _, s := range standOut(ms, nil, 2*time.Second) {
		t.Errorf("%s stands out for %q, with every member short", s.Member, s.Why)
	}
}

// Right after a start, a cache filling to its size grows as fast as a leak:
// b, 21 s up at half its limit, said "Running short ... at the limit in
// about 17 s", with its kill minutes away. While starting, only the level
// counts, unless the former process was killed for memory while starting
// too: a cache that doesn't fit (the memory evals' case a) dies 20 to 30 s after
// every start, and its forecast is then the warning.
func TestAStartingProcesssMemoryTrendIsTrustedOnlyInACrashLoop(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	filling := RuntimeStats{MemUsed: mib(270), MemLimit: mib(512), MemGrowth: 15 * (1 << 20)}
	if got := memoryPressure(filling, 0); got != "" {
		t.Errorf("starting, at half its limit: %q", got)
	}
	if got := memoryPressure(filling, pressureAhead); got == "" {
		t.Error("in a crash loop, filling toward the limit in 17 s: no pressure")
	}
	full := RuntimeStats{MemUsed: mib(460), MemLimit: mib(512), MemGrowth: 4 * (1 << 20)}
	if got := memoryPressure(full, 0); got == "" {
		t.Error("starting, at 90%: no pressure")
	}

	start := time.Now()
	for _, c := range []struct {
		up     time.Duration
		former *formerProcess
		want   bool
	}{
		{20 * time.Second, nil, false},
		{20 * time.Second, &formerProcess{died: true, forMemory: true, up: 4 * time.Minute}, false}, // a leak's kill, minutes in
		{20 * time.Second, &formerProcess{died: true, forMemory: true, up: 25 * time.Second}, true}, // killed while starting
		{90 * time.Second, nil, true},
	} {
		if got := trendTrusted(start, start.Add(c.up), c.former); got != c.want {
			t.Errorf("up %v, former %+v: trusted %v", c.up, c.former, got)
		}
	}
}

// b's forecast hovered about two minutes from the limit, and its pressure
// came and went every 5 to 10 s: on at two minutes, it stays on to three.
func TestMemoryPressureHoldsUntilTheLimitIsThreeMinutesOff(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	rt := RuntimeStats{MemUsed: mib(362), MemLimit: mib(512), MemGrowth: 1 * (1 << 20)} // 150 s off
	if got := memoryPressure(rt, pressureAhead); got != "" {
		t.Errorf("150 s off, not short before: %q", got)
	}
	if got := memoryPressure(rt, pressureHold); got == "" {
		t.Error("150 s off, short before: no pressure")
	}
}

// The page drew memory only as text, and a leak's slope is a line's: the
// state carries the container's memory over the last 10 minutes, a point
// each 10 s, from the runtime samples the instance keeps.
func TestTheContainersMemoryIsKeptForTheLast10Minutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		FakeCgroup(t, map[string]string{
			"cpu.max": "max 100000\n", "cpu.stat": "usage_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n",
			"memory.current": "524288000\n", "memory.max": "536870912\n", "memory.stat": "inactive_file 0\n", "memory.events": "oom_kill 0\n",
		})
		in := NewInstance()
		for range 12 * 60 { // 12 minutes, read each second as the page does
			in.Runtime(10 * time.Second)
			time.Sleep(time.Second)
		}
		h := in.memHistory()
		if len(h) < 59 || len(h) > 61 {
			t.Fatalf("%d points, want about 60: a point each 10 s over 10 minutes", len(h))
		}
		if h[0].UsedMB != 500 || h[len(h)-1].UsedMB != 500 {
			t.Errorf("points %+v ... %+v, want 500 MiB", h[0], h[len(h)-1])
		}
		if h[len(h)-1].HeapMB <= 0 { // the live heap too: what of it grew in the window is drawn apart
			t.Errorf("no live heap in %+v", h[len(h)-1])
		}
		if !h[0].At.Before(h[len(h)-1].At) {
			t.Error("not oldest first")
		}
	})
}

// The memory figure draws the forecast to the limit, as pressure reads it:
// none while a starting process's trend isn't trusted, none when memory
// isn't heading there.
func TestTheMemoryFigureForecastsAsPressureDoes(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	filling := RuntimeStats{MemUsed: mib(270), MemLimit: mib(512), MemGrowth: 15 * (1 << 20)}
	if got := limitIn(filling, false); got >= 0 {
		t.Errorf("starting: a forecast of %v", got)
	}
	if got := limitIn(filling, true); got < 15*time.Second || got > 17*time.Second {
		t.Errorf("trusted, 242 MiB at 15 MiB/s: %v, want about 16 s", got)
	}
	if got := limitIn(RuntimeStats{MemUsed: mib(300), MemLimit: mib(512)}, true); got >= 0 {
		t.Errorf("steady: a forecast of %v", got)
	}
}
