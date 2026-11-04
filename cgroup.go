package vis

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Where cgroup v2 lives. Variables so tests can point them at fixtures; guarded
// because instances read them from background goroutines.
var (
	cgroupMu       sync.RWMutex
	cgroupRoot     = "/sys/fs/cgroup"
	procSelfCgroup = "/proc/self/cgroup"
)

func cgroupPaths() (root, self string) {
	cgroupMu.RLock()
	defer cgroupMu.RUnlock()
	return cgroupRoot, procSelfCgroup
}

func setCgroupPaths(root, self string) {
	cgroupMu.Lock()
	defer cgroupMu.Unlock()
	cgroupRoot, procSelfCgroup = root, self
}

// cgroupStat is one reading of the process's cgroup: its CPU limit and the
// cumulative counters from cpu.stat. Zero limit means none.
type cgroupStat struct {
	limit         float64 // cores, from cpu.max
	periods       uint64  // scheduling periods so far
	throttled     uint64  // periods in which the cgroup used up its quota and was paused
	throttledTime time.Duration
	usage         time.Duration // CPU time used

	// Memory, from memory.current, memory.stat, memory.max and memory.events.
	memUsed  uint64 // the working set: in use, less the inactive page cache the kernel reclaims before it kills
	memLimit uint64 // memory.max; 0 when unlimited
	oomKills uint64 // processes the kernel killed in this cgroup for memory, since the cgroup began
}

// readCgroup reads the process's own cgroup, found through /proc/self/cgroup,
// so a process outside a container doesn't report the whole host. It reports
// false where there is no cgroup v2, such as on macOS.
func readCgroup() (cgroupStat, bool) {
	root, selfPath := cgroupPaths()
	self, err := os.ReadFile(selfPath)
	if err != nil {
		return cgroupStat{}, false
	}
	var path string
	for _, line := range strings.Split(string(self), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok { // the v2 hierarchy
			path = p
		}
	}
	if path == "" {
		return cgroupStat{}, false
	}
	dir := filepath.Join(root, path)
	f, err := os.Open(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return cgroupStat{}, false
	}
	defer f.Close()
	var c cgroupStat
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, val, _ := strings.Cut(sc.Text(), " ")
		n, _ := strconv.ParseUint(val, 10, 64)
		switch name {
		case "nr_periods":
			c.periods = n
		case "nr_throttled":
			c.throttled = n
		case "throttled_usec":
			c.throttledTime = time.Duration(n) * time.Microsecond
		case "usage_usec":
			c.usage = time.Duration(n) * time.Microsecond
		}
	}
	if max, err := os.ReadFile(filepath.Join(dir, "cpu.max")); err == nil {
		quota, period, _ := strings.Cut(strings.TrimSpace(string(max)), " ")
		q, qerr := strconv.ParseFloat(quota, 64) // "max" means no limit
		p, perr := strconv.ParseFloat(period, 64)
		if qerr == nil && perr == nil && p > 0 {
			c.limit = q / p
		}
	}
	c.readMemory(dir)
	return c, true
}

// readMemory fills in the cgroup's memory. A file that isn't there (a memory
// controller not enabled) leaves its reading at zero.
func (c *cgroupStat) readMemory(dir string) {
	num := func(name string) (uint64, bool) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64) // "max" means no limit
		return n, err == nil
	}
	field := func(name, key string) uint64 {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, " "); ok && k == key {
				n, _ := strconv.ParseUint(v, 10, 64)
				return n
			}
		}
		return 0
	}
	if cur, ok := num("memory.current"); ok {
		c.memUsed = cur - min(cur, field("memory.stat", "inactive_file"))
	}
	c.memLimit, _ = num("memory.max")
	c.oomKills = field("memory.events", "oom_kill")
}

// addMemory fills in the container's memory from one reading: it is a level, not a rate.
func (s *RuntimeStats) addMemory(c *cgroupStat) {
	s.MemUsed, s.MemLimit, s.OOMKills = c.memUsed, c.memLimit, c.oomKills
}

// addCgroup fills in the container signals for a window between two readings.
func (s *RuntimeStats) addCgroup(older, newer *cgroupStat, over time.Duration) {
	s.CPULimit = newer.limit
	if p := newer.periods - older.periods; p > 0 {
		s.Throttled = float64(newer.throttled-older.throttled) / float64(p)
	}
	s.ThrottledTime = newer.throttledTime - older.throttledTime
	if over > 0 {
		s.CPUUsed = float64(newer.usage-older.usage) / float64(over)
	}
}
