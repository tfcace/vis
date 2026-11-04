package vis

import (
	"math"
	"runtime/debug"
	"strings"
	"testing"
)

// noCgroup runs a test as a process outside any cgroup: a VM, a Mac, Lambda.
func noCgroup(t *testing.T) {
	oldRoot, oldSelf := cgroupPaths()
	setCgroupPaths(t.TempDir(), "/nonexistent/cgroup")
	t.Cleanup(func() { setCgroupPaths(oldRoot, oldSelf) })
	t.Setenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "")
	old := debug.SetMemoryLimit(math.MaxInt64)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
}

// vis isn't only for containers: outside a cgroup it reads the Go runtime's
// own memory, so the dashboard and the forecast still have a reading.
func TestOutsideACgroupMemoryIsTheProcesss(t *testing.T) {
	noCgroup(t)
	rt := NewInstance().Runtime(10 * 1e9)
	if rt.MemUsed == 0 || rt.MemFrom != "process" {
		t.Fatalf("used %d MiB from %q, want the process's own memory", rt.MemUsed>>20, rt.MemFrom)
	}
	if rt.MemLimit != 0 || rt.MemLimitBy != "" {
		t.Errorf("a limit of %d MiB by %q with none set", rt.MemLimit>>20, rt.MemLimitBy)
	}
}

// On Lambda the function's memory size is where it is killed.
func TestOnLambdaTheFunctionsMemorySizeIsTheLimit(t *testing.T) {
	noCgroup(t)
	t.Setenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE", "512")
	rt := NewInstance().Runtime(10 * 1e9)
	if rt.MemLimit != 512<<20 || rt.MemLimitBy != "lambda" || !rt.memKills() {
		t.Fatalf("limit %d MiB by %q (kills %v), want Lambda's 512, which kills", rt.MemLimit>>20, rt.MemLimitBy, rt.memKills())
	}
}

// With no limit but GOMEMLIMIT, GOMEMLIMIT is the line memory runs into:
// the collector runs almost without pause there, but nothing kills it.
func TestGOMEMLIMITStandsInForAMissingLimit(t *testing.T) {
	noCgroup(t)
	debug.SetMemoryLimit(400 << 20)
	rt := NewInstance().Runtime(10 * 1e9)
	if rt.MemLimit != 400<<20 || rt.MemLimitBy != "GOMEMLIMIT" || rt.memKills() {
		t.Fatalf("limit %d MiB by %q (kills %v), want GOMEMLIMIT's 400, which doesn't kill", rt.MemLimit>>20, rt.MemLimitBy, rt.memKills())
	}
	if m := NewInstance().memDigest(rt); m == nil || m.Limit != 0 {
		t.Errorf("digest %+v: a limit that doesn't kill can't explain a death", m)
	}
}

// Each source says its own words: the container or the process, and which limit.
func TestMemoryIsSaidAsItsSourceIs(t *testing.T) {
	mib := func(n float64) uint64 { return uint64(n * (1 << 20)) }
	for _, c := range []struct {
		rt   RuntimeStats
		want []string
		not  []string
	}{
		{RuntimeStats{MemFrom: "container", MemUsed: mib(460), MemLimit: mib(512), MemLimitBy: "cgroup"},
			[]string{"the container uses 460 of its 512 MiB limit"}, nil},
		{RuntimeStats{MemFrom: "process", MemUsed: mib(460), MemLimit: mib(512), MemLimitBy: "lambda"},
			[]string{"the process uses 460 of its 512 MiB Lambda memory size"}, nil},
		{RuntimeStats{MemFrom: "process", MemUsed: mib(380), MemLimit: mib(400), MemLimitBy: "GOMEMLIMIT", GoMemLimit: mib(400),
			HeapLive: mib(200), HeapGoal: mib(380), HeapGrowth: 3 << 20},
			[]string{"the process uses 380 of its 400 MiB GOMEMLIMIT", "the collector runs almost without pause"}, []string{"killed", "container"}},
	} {
		got := memoryPressure(c.rt, pressureAhead)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%q\nwant %q", got, w)
			}
		}
		for _, w := range c.not {
			if strings.Contains(got, w) {
				t.Errorf("%q\nsays %q", got, w)
			}
		}
	}
}
