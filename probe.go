package vis

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Probe times part of a request, from the call until done:
//
//	defer vis.Probe(ctx, "origin")()
//
// For each route vis then reports how long each probe takes, and groups the
// requests by the probes they ran, so a cache hit and a miss (requests that
// ran "lookup", and those that ran "lookup" then "origin") show as two
// latencies with no other declaration. A probe run more than once in a request
// counts its total time. Name probes with constants: each route keeps 15 and
// counts the rest as "other". With defer the probe runs until the function
// returns; for part of a function, call done where that part ends.
// Outside a request served by Instance.HTTP (or a framework adapter) a probe
// does nothing, so library code can always call it.
func Probe(ctx context.Context, name string) (done func()) {
	rp, _ := ctx.Value(probesKey{}).(*reqProbes)
	if rp == nil {
		return func() {}
	}
	i := rp.begin(name) // ordered by when probes start: a probe wrapping another comes first
	start := time.Now()
	return func() { rp.add(i, time.Since(start)) }
}

type probesKey struct{}

// requestCtx carries a request's probes without a second allocation for a context.WithValue.
type requestCtx struct {
	context.Context
	probes reqProbes
}

func (c *requestCtx) Value(key any) any {
	if key == (probesKey{}) {
		return &c.probes
	}
	return c.Context.Value(key)
}

// reqProbes is one request's probes, in the order they first ran. A request
// may run probes on more than one goroutine.
type reqProbes struct {
	mu    sync.Mutex
	names []string
	total []time.Duration
}

func (rp *reqProbes) begin(name string) int {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	for i, n := range rp.names {
		if n == name {
			return i
		}
	}
	rp.names = append(rp.names, name)
	rp.total = append(rp.total, 0)
	return len(rp.names) - 1
}

func (rp *reqProbes) add(i int, d time.Duration) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.total[i] += d
}

// Each route keeps its own probes and paths, a few of each: probe names are
// meant to be constants, and any beyond the cap (names built from input, say)
// count under "other" on that route alone.
const (
	maxProbes = 15 // plus "other"
	maxPaths  = 15
	stepSep   = "\x00" // between a path's probes
)

type probeTable struct {
	mu      sync.Mutex
	byRoute map[string]*routeProbes
}

type routeProbes struct {
	probes routeTable // by probe name
	paths  routeTable // by the probes a request ran, joined
}

func (pt *probeTable) route(name string, create bool) *routeProbes {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	rp := pt.byRoute[name]
	if rp == nil && create {
		if pt.byRoute == nil {
			pt.byRoute = map[string]*routeProbes{}
		}
		rp = &routeProbes{probes: routeTable{max: maxProbes}, paths: routeTable{max: maxPaths}}
		pt.byRoute[name] = rp
	}
	return rp
}

// recordProbes records a finished request's probes under route, the name the
// routes table filed the request under ("other" past MaxRoutes).
func (in *Instance) recordProbes(route string, rp *reqProbes, d time.Duration, status int, now time.Time) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if len(rp.names) == 0 {
		return
	}
	t := in.probes.route(route, true)
	for i, n := range rp.names {
		t.probes.record(n, rp.total[i], http.StatusOK, now) // a probe's own time: its request's outcome is the path's
	}
	t.paths.record(strings.Join(rp.names, stepSep), d, status, now)
}

// Breakdown is where a route's requests spend their time, over a window.
type Breakdown struct {
	Route  string
	Probes []ProbeStats // in the order they first ran
	Paths  []PathStats  // the most requests first
}

// ProbeStats is one probe on one route: the requests that ran it and the time they spent in it.
type ProbeStats struct {
	Probe    string
	Requests int64
	Share    float64 // of the route's requests
	P50, P99 time.Duration
}

// PathStats is the requests that ran the same probes, and their whole latency.
type PathStats struct {
	Probes   []string
	Requests int64
	Share    float64
	Errors   int64
	P50, P99 time.Duration
}

// Breakdown says where a route's requests spent their time over the last
// window, up to 5 minutes, from the probes they ran.
func (in *Instance) Breakdown(route string, window time.Duration) Breakdown {
	if route == "" {
		route = unmatchedRoute
	}
	now := time.Now()
	b := Breakdown{Route: route}
	t := in.probes.route(route, false)
	if t == nil {
		return b
	}
	total := in.routes.stats(route, window, now).Requests
	share := func(n int64) float64 {
		if total == 0 {
			return 0
		}
		return float64(n) / float64(total)
	}
	for _, name := range t.probes.names() {
		if s := t.probes.stats(name, window, now); s.Requests > 0 {
			b.Probes = append(b.Probes, ProbeStats{Probe: name, Requests: s.Requests, Share: share(s.Requests), P50: s.P50, P99: s.P99})
		}
	}
	for _, key := range t.paths.names() {
		if s := t.paths.stats(key, window, now); s.Requests > 0 {
			b.Paths = append(b.Paths, PathStats{Probes: strings.Split(key, stepSep), Requests: s.Requests, Share: share(s.Requests),
				Errors: s.Errors, P50: s.P50, P99: s.P99})
		}
	}
	sort.SliceStable(b.Paths, func(i, j int) bool { return b.Paths[i].Requests > b.Paths[j].Requests })
	return b
}
