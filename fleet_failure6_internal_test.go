package vis

// These tests came from a fourth critique of the fleet, from what agents did in the evals.

import (
	"strings"
	"testing"
	"time"
)

// A coordinator starved of CPU waits on everyone: a peer that didn't answer
// it in time, and whose own gossip says it is healthy, isn't the culprit.
// In sick-coordinator, agents chased such a peer and told the person to look
// at it.
func TestAStarvedCoordinatorDoesntBlameAHealthyPeer(t *testing.T) {
	ms := []FleetMember{
		{Member: Member{Name: "cp-1", Self: true}, Throttled: 1, SLOs: []MemberSLO{{Route: "GET /k", P99: 2800 * time.Millisecond, Requests: 500}}},
		{Member: Member{Name: "cp-2"}, Gossiped: 2 * time.Second, SLOs: []MemberSLO{{Route: "GET /k", P99: 20 * time.Millisecond, Requests: 500, Met: true}}},
		{Member: Member{Name: "cp-3"}, SLOs: []MemberSLO{{Route: "GET /k", P99: 20 * time.Millisecond, Requests: 500, Met: true}}},
	}
	var self, peer string
	for _, s := range standOut(ms, nil, 2*time.Second) {
		switch s.Member {
		case "cp-1":
			self = s.Why
		case "cp-2":
			peer = s.Why
		}
	}
	if peer != "" {
		t.Errorf("cp-2, healthy by its own gossip, stands out: %q", peer)
	}
	if !strings.Contains(self, "cp-2") || !strings.Contains(self, "likely here") {
		t.Errorf("cp-1's reason = %q; want it to say cp-2 didn't answer it in time because the wait is likely here", self)
	}
}

// A starved peer that can't answer is still the likely culprit when this
// instance is fine.
func TestAPeerTooBusyToAnswerStillStandsOut(t *testing.T) {
	ms := []FleetMember{
		{Member: Member{Name: "cp-1", Self: true}, SLOs: []MemberSLO{{Route: "GET /k", P99: 20 * time.Millisecond, Requests: 500, Met: true}}},
		{Member: Member{Name: "cp-2"}, Gossiped: 2 * time.Second, Throttled: 1, SLOs: []MemberSLO{{Route: "GET /k", P99: 2800 * time.Millisecond, Requests: 500}}},
	}
	so := standOut(ms, nil, 2*time.Second)
	if len(so) != 1 || so[0].Member != "cp-2" || !strings.Contains(so[0].Why, "likely culprit") {
		t.Errorf("stand_out = %+v; want cp-2, too busy to answer", so)
	}
}

// The canary's evidence says in a word what the change did, and what it did
// to the CPU, which under a squeeze says more than a p99.
func TestTheCanarysEvidenceSaysItHelped(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{
		Before: map[string]float64{"GET /k p99": 2800, "GET /k requests": 500, "GET /k errors": 0, "cpu_used": .25, "throttled": 1},
		After:  map[string]float64{"GET /k p99": 18, "GET /k requests": 500, "GET /k errors": 0, "cpu_used": .06, "throttled": 0},
		Rest: &Rest{Members: []string{"a", "c"},
			Before: map[string]float64{"GET /k p99": 2800, "GET /k requests": 1000},
			After:  map[string]float64{"GET /k p99": 2900, "GET /k requests": 1000}}}
	harm, unjudged, ev := gate(im, objs, "b")
	if harm != "" || unjudged != "" || !strings.HasPrefix(ev, "helped: ") || !strings.Contains(ev, "throttled 100% → 0%") || !strings.Contains(ev, "CPU 0.25 → 0.06 cores") {
		t.Errorf("harm=%q unjudged=%q evidence=%q; want helped, with the CPU it saved", harm, unjudged, ev)
	}
}

// A member changed seconds ago shows numbers from before its change: the
// fleet view says so, rather than flag the fix as what spends the CPU. In
// fleet-squeeze, an agent reading the fleet right after its rollout had to
// reason out that it was a backlog draining.
func TestAFreshChangeSaysTheNumbersPredateIt(t *testing.T) {
	ms := []FleetMember{
		{Member: Member{Name: "cp-1", Self: true}, Throttled: 1, LastChange: "compress_level 9 → 1 by agent:oncall", LastAgo: 3 * time.Second,
			SLOs: []MemberSLO{{Route: "GET /k", P99: 2300 * time.Millisecond, Requests: 500}}},
		{Member: Member{Name: "cp-2"}, SLOs: []MemberSLO{{Route: "GET /k", P99: 20 * time.Millisecond, Requests: 500, Met: true}}},
	}
	so := standOut(ms, nil, 2*time.Second)
	if len(so) != 1 || !strings.Contains(so[0].Why, "changed 3s ago") || !strings.Contains(so[0].Why, "mostly from before it") {
		t.Errorf("stand_out = %+v; want cp-1's numbers said to predate its change", so)
	}
}
