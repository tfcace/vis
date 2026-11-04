package vis

// These tests came from a third review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tfcace/vis/internal/pipenet"
)

// G1: the rest's ratio is taken from a handful of requests: two slow ones on a
// nearly idle rest hide the first member's p99 tripling.
func TestTheGateIgnoresARestOfAHandfulOfRequests(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 500}, After: map[string]float64{"GET /k p99": 30, "GET /k requests": 500},
		Rest: &Rest{Members: []string{"a", "c"},
			Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 4},
			After:  map[string]float64{"GET /k p99": 39, "GET /k requests": 3}}}
	harm, unjudged, ev := gate(im, objs, "b")
	t.Logf("harm=%q unjudged=%q evidence=%q", harm, unjudged, ev)
	if harm == "" {
		t.Errorf("b's p99 10 -> 30 ms (3x) passes because 7 requests on the rest went 10 -> 39 ms")
	}
}

// G2: the rest were changed themselves in the window (Rest.Changed); their own
// change slowed them, and the gate reads that as something shared.
func TestTheGateIgnoresARestThatWasChangedToo(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 500}, After: map[string]float64{"GET /k p99": 19, "GET /k requests": 500},
		Rest: &Rest{Members: []string{"a", "c"},
			Before:  map[string]float64{"GET /k p99": 10, "GET /k requests": 500},
			After:   map[string]float64{"GET /k p99": 20, "GET /k requests": 500},
			Changed: []string{"a: cache_size 1000 → 10 by human:sam", "c: cache_size 1000 → 10 by human:sam"}}}
	harm, unjudged, ev := gate(im, objs, "b")
	t.Logf("harm=%q unjudged=%q evidence=%q", harm, unjudged, ev)
	if harm == "" && unjudged == "" && !strings.Contains(ev, "cache_size") {
		t.Errorf("b's p99 +90%% passes against a rest slowed by its own change, and the evidence doesn't say they were changed")
	}
}

// G3: the change stops the route answering on the first member: 500 requests
// before, none completed after, while the rest serve as usual. The gate calls
// it too few requests to judge, and the rollout says "no harm seen" and keeps it.
func TestARouteThatStopsAnsweringIsHarm(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 500, "GET /k errors": 0}, After: map[string]float64{"cpu_used": 0.01},
		Rest: &Rest{Members: []string{"a", "c"},
			Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 1000},
			After:  map[string]float64{"GET /k p99": 10, "GET /k requests": 1000}}}
	harm, unjudged, ev := gate(im, objs, "b")
	t.Logf("harm=%q unjudged=%q evidence=%q", harm, unjudged, ev)
	if harm == "" {
		t.Errorf("500 requests -> none, while the rest served 1000: not harm (unjudged %q)", unjudged)
	}
}

// C1: one member holds a value under a pin with no end, the others under a
// 5-minute lease: common says it holds "with no end" on every member.
func TestCommonSaysMixedEndsTruthfully(t *testing.T) {
	ms := []FleetMember{{Member: Member{Name: "a"}}, {Member: Member{Name: "b"}}, {Member: Member{Name: "c"}}}
	sums := []*peerSummary{
		{Held: []Held{{Knob: "compress_level", Value: "1", By: "human:sam", EndsIn: 0}}},
		{Held: []Held{{Knob: "compress_level", Value: "1", By: "agent:oncall", EndsIn: 5 * time.Minute}}},
		{Held: []Held{{Knob: "compress_level", Value: "1", By: "agent:oncall", EndsIn: 29 * time.Minute}}},
	}
	got := strings.Join(common(ms, sums), "\n")
	t.Logf("common: %s", got)
	if strings.HasSuffix(got, ", with no end") {
		t.Errorf("says no end, though b's ends in 5m and c's in 29m")
	}
	sums[0].Held[0] = Held{Knob: "compress_level", Value: "1", By: "agent:oncall", EndsIn: time.Minute}
	got = strings.Join(common(ms, sums), "\n")
	t.Logf("common: %s", got)
	if strings.Contains(got, "about at once") {
		t.Errorf("1m to 29m read as about at once")
	}
}

// C2: every member is short of CPU, and one too busy to answer is read from its
// gossip, which carries no pressures: common says nothing about the squeeze.
func TestCommonKeepsAPressureWhenOneMemberIsGossiped(t *testing.T) {
	ms := []FleetMember{{Member: Member{Name: "a"}}, {Member: Member{Name: "b"}, Gossiped: 3 * time.Second}, {Member: Member{Name: "c"}}}
	p := []Pressure{{Resource: CPU, Why: "throttled"}}
	gossip := (&wireHealth{Held: nil}).summary(Member{Name: "b"})
	sums := []*peerSummary{{Pressures: p}, gossip, {Pressures: p}}
	got := common(ms, sums)
	t.Logf("common: %q", got)
	if len(got) == 0 {
		t.Errorf("every member that answered is short of CPU, and b is too busy to answer: common says nothing")
	}
	// and a silent member: "every member" from two of three
	ms[1].Gossiped, ms[1].Silent = 0, true
	sums[1] = nil
	t.Logf("with b silent: %q", common(ms, sums))
}

// F1: a page read while the first (cold) comparison runs gets an empty report:
// no members, so the page shows a single instance.
func TestAReadDuringTheFirstRefreshWaitsForIt(t *testing.T) {
	n := pipenet.New()
	in := NewInstance(Service("s"), InstanceName("a"))
	r := bareRing(t, in, n)
	r.wait = 300 * time.Millisecond
	ln, _ := n.Listen("b:7946")
	go func() { // accepts, never answers
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				b := make([]byte, 1<<16)
				for {
					if _, err := c.Read(b); err != nil {
						return
					}
				}
			}()
		}
	}()
	defer ln.Close()
	r.members["b"] = &memberEntry{m: wireMember{ID: "b", Name: "b", Addr: "b:7946", Service: "s"}, rose: time.Now()}
	var wg sync.WaitGroup
	wg.Go(func() { r.cachedFleet(context.Background()) })
	time.Sleep(50 * time.Millisecond)
	rep := r.cachedFleet(context.Background())
	wg.Wait()
	out := fleetJSON(rep)
	t.Logf("second read: members=%d covers=%q age=%q", len(out.Members), out.Covers, out.Age)
	if len(out.Members) < 2 {
		t.Errorf("a read during the first refresh shows %d members: the page takes it for a lone instance", len(out.Members))
	}
}

// F2: a page request in flight at shutdown kicks a refresh while the ring waits for them.
func TestNoRefreshStartsAsTheRingStops(t *testing.T) {
	n := pipenet.New()
	in := NewInstance(Service("s"), InstanceName("a"))
	r := bareRing(t, in, n)
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	r.fleetAsked = time.Now()
	cancel()
	var wg sync.WaitGroup
	for range 200 {
		r.fleetMu.Lock()
		r.fleetAt = time.Time{}
		r.fleetMu.Unlock()
		wg.Go(func() { r.kickFleet() }) // the page's handler, still running
		r.fleetMu.Lock()                // the ring's shutdown
		r.fleetShut = true
		r.fleetMu.Unlock()
		r.bg.Wait()
		r.fleetMu.Lock()
		r.fleetShut = false
		r.fleetMu.Unlock()
	}
	wg.Wait()
	r.bg.Wait()
}
