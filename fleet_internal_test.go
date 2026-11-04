package vis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// What differs about a member is where its trouble likely comes from: a CPU
// limit or a build the rest don't share, said plainly.
func TestDifferencesNameTheOddMemberOut(t *testing.T) {
	m := func(name string, limit float64, build string) FleetMember {
		return FleetMember{Member: Member{Name: name, Build: build, State: "alive"}, CPULimit: limit}
	}
	ms := []FleetMember{m("a", 2, "abc1234"), m("b", .25, "def5678"), m("c", 2, "abc1234"), {Member: Member{Name: "d", Build: "zzz", State: "dead"}, Silent: true}}
	diffs := differences(ms, make([]*peerSummary, len(ms)))
	var what []string
	for _, d := range diffs {
		what = append(what, d.What)
	}
	if !slices.Equal(what, []string{"cpu_limit", "build"}) {
		t.Fatalf("differences = %v, want the CPU limit and the build, and nothing from the silent member", what)
	}
	got := causes(ms[1], diffs)
	want := []string{"its container's CPU limit is 0.25 cores, while the others have 2 cores", "it runs build def5678, while the others run abc1234"}
	if !slices.Equal(got, want) {
		t.Fatalf("causes for b = %q, want %q", got, want)
	}
	if got := causes(ms[0], diffs); len(got) != 0 {
		t.Fatalf("causes for a = %q, want none: it has what the rest have", got)
	}
}

// A freeze is the safety switch, so across a partition it wins: a freeze set
// after the unfreeze that would lift it stands, and spreads; equal counts of
// operations go to frozen.
func TestAFreezeSetLaterSurvivesAPartition(t *testing.T) {
	sam, kim := Writer{Kind: Human, ID: "sam"}, Writer{Kind: Human, ID: "kim"}
	t0 := time.Now()
	cases := []struct {
		why        string
		local, got FreezeState
		wantFrozen bool
	}{
		{"an unfreeze from before this side's freeze", FreezeState{Frozen: true, By: kim, Since: t0.Add(2 * time.Minute), Epoch: 4, origin: "b"},
			FreezeState{By: sam, Since: t0.Add(time.Minute), Epoch: 5, origin: "a"}, true},
		{"a freeze from after this side's unfreeze", FreezeState{By: sam, Since: t0.Add(time.Minute), Epoch: 5, origin: "a"},
			FreezeState{Frozen: true, By: kim, Since: t0.Add(2 * time.Minute), Epoch: 4, origin: "b"}, true},
		{"the same count either way", FreezeState{By: sam, Since: t0, Epoch: 4, origin: "z"},
			FreezeState{Frozen: true, By: kim, Since: t0, Epoch: 4, origin: "a"}, true},
		{"an unfreeze after the freeze", FreezeState{Frozen: true, By: kim, Since: t0, Epoch: 2, origin: "b"},
			FreezeState{By: sam, Since: t0.Add(time.Minute), Epoch: 3, origin: "a"}, false},
	}
	for _, c := range cases {
		in := NewInstance()
		in.freeze = c.local
		in.adoptFreeze(c.got)
		f := in.FreezeState()
		if f.Frozen != c.wantFrozen {
			t.Errorf("%s: frozen = %v, want %v", c.why, f.Frozen, c.wantFrozen)
		}
		if f.Epoch <= max(c.local.Epoch, c.got.Epoch) && c.wantFrozen && c.local.Epoch != c.got.Epoch {
			t.Errorf("%s: epoch %d, want above both sides' so the outcome spreads", c.why, f.Epoch)
		}
	}
}

// A signed peer request is good once: a captured change sent again within the
// minute its signature is valid is refused.
func TestASignedPeerRequestIsGoodOnce(t *testing.T) {
	r := &ring{in: NewInstance(), key: []byte("k"), self: wireMember{Service: "service"}}
	body := []byte(`{"set":{"x":1}}`)
	req := httptest.NewRequest("POST", "http://b:7946"+peerPath+"ui/api/apply", bytes.NewReader(body))
	r.sign(req, body)
	if err := r.verify(req, body); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := r.verify(req, body); err == nil || !strings.Contains(err.Error(), "replayed") {
		t.Fatalf("again: %v, want it refused as replayed", err)
	}
}

// A gossip message carries the sender's own histograms, and only a p99 for
// each member it relays: what every member relays every second grows with
// the square of the fleet, so it must stay small.
func TestRelayedGossipCarriesNoHistograms(t *testing.T) {
	var h hist
	for i := range 20000 {
		h.add(time.Duration(1+i%3000) * time.Millisecond)
	}
	var pairs [][2]uint32
	for i, c := range h.counts {
		if c > 0 {
			pairs = append(pairs, [2]uint32{uint32(i), c})
		}
	}
	r := &ring{in: NewInstance(), members: map[string]*memberEntry{}}
	r.self = wireMember{ID: "self", Name: "a", Service: "s", Health: &wireHealth{SLOs: []peerSLO{{Route: "GET /", Hist: pairs, P99: time.Second}}}}
	for i := range 30 {
		id := fmt.Sprint("m", i)
		r.members[id] = &memberEntry{rose: time.Now(), m: wireMember{ID: id, Name: id, Service: "s",
			Health: &wireHealth{SLOs: []peerSLO{{Route: "GET /", Hist: pairs, P99: time.Second}}}}}
	}
	msg := r.list()
	if len(msg.Members[0].Health.SLOs[0].Hist) == 0 {
		t.Fatal("the sender's own histograms are gone")
	}
	for _, m := range msg.Members[1:] {
		if s := m.Health.SLOs[0]; len(s.Hist) != 0 || s.P99 != time.Second {
			t.Fatalf("relayed %s carries %d buckets, p99 %v; want none, and its p99", m.Name, len(s.Hist), s.P99)
		}
	}
	b, _ := json.Marshal(msg)
	if len(b) > 12<<10 {
		t.Fatalf("a message relaying 30 members is %d KB, want under 12", len(b)>>10)
	}
}

// A member restarted moments ago shows twice until its former self goes
// quiet: whatever acts on "each live member" must take it once.
func TestLiveMembersAreOncePerName(t *testing.T) {
	ms := []Member{{Name: "a", Self: true, State: "alive"}, {Name: "b", ID: "old", State: "alive", Quiet: 3 * time.Second},
		{Name: "b", ID: "new", State: "alive", Quiet: 100 * time.Millisecond}, {Name: "c", State: "dead"}}
	got := liveOnce(ms[1:])
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("live once = %+v, want only b's newest", got)
	}
}

// The gate needs enough requests in both windows, and a clean window.
func TestTheGateNeedsRequestsAndACleanWindow(t *testing.T) {
	objs := []sloDef{{"GET /k", 50 * time.Millisecond}}
	quiet := Impact{Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 5}, After: map[string]float64{"GET /k p99": 10, "GET /k requests": 5}}
	if _, unjudged, _ := gate(quiet, objs, "b"); !strings.Contains(unjudged, "too few requests") {
		t.Errorf("5 requests a window: %q, want too few to judge", unjudged)
	}
	busy := Impact{Before: map[string]float64{"GET /k p99": 10, "GET /k requests": 500}, After: map[string]float64{"GET /k p99": 10, "GET /k requests": 500},
		ConfoundedBy: []Confounder{{What: "cache_size changed (1000 → 100) by human:sam", Kind: "change", Layer: "pin"}}}
	if _, unjudged, _ := gate(busy, objs, "b"); !strings.Contains(unjudged, "confounded") {
		t.Errorf("a window a person changed something in: %q, want the gate unable to judge", unjudged)
	}
}

// A member's own rule reacting during the window (in a fault on every member,
// every member's rules are) doesn't stop the gate: it's the system working,
// not someone else's change. The fleet-squeeze round undid a working fix for it.
func TestTheGateLetsAMembersRulesReact(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 285, "GET /k requests": 500}, After: map[string]float64{"GET /k p99": 18, "GET /k requests": 900},
		ConfoundedBy: []Confounder{{What: "batch_size changed (48 → 36) by rule:batch_backoff", Kind: "change", Layer: "rule"}}}
	harm, unjudged, evidence := gate(im, objs, "cp-3")
	if harm != "" || unjudged != "" || !strings.Contains(evidence, "batch_backoff") {
		t.Fatalf("harm %q, unjudged %q, evidence %q; want it judged harmless, the rule's move noted", harm, unjudged, evidence)
	}
}

// Everything a member's own rules do in the window is the system at work: a
// rule's step, its lease ending, its revert. Round-20261103-0109's run
// 012432 stopped on a rule's lease ending ("batch_size changed (20 → 64) by
// lease"), which a test of the rule's step alone missed.
func TestTheGateLetsARulesLeaseEnd(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 285, "GET /k requests": 500}, After: map[string]float64{"GET /k p99": 18, "GET /k requests": 900},
		ConfoundedBy: []Confounder{{What: "batch_size changed (20 → 64) by lease", Kind: "change", Layer: "rule"}}}
	if harm, unjudged, _ := gate(im, objs, "cp-3"); harm != "" || unjudged != "" {
		t.Fatalf("harm %q, unjudged %q; want a rule's lease ending noted, not stopping the gate", harm, unjudged)
	}
}

// Harm the member can't hide behind a rest getting worse: its own p99 doubling
// to over its objective, or its errors doubling, is harm whatever the rest did.
// And a window the gate can't judge still says what it saw, harm included.
func TestTheGateSeesHarmWhateverTheRestDid(t *testing.T) {
	objs := []sloDef{{"GET /k", 40 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 100, "GET /k requests": 500}, After: map[string]float64{"GET /k p99": 400, "GET /k requests": 500},
		Rest: &Rest{Members: []string{"a", "c"}, Before: map[string]float64{"GET /k p99": 200}, After: map[string]float64{"GET /k p99": 2000}}}
	if harm, _, _ := gate(im, objs, "b"); harm == "" {
		t.Fatal("p99 100 → 400 ms, over the objective, passed because the rest went 200 → 2000")
	}
	im.ConfoundedBy = []Confounder{{What: "cache_size changed (1000 → 10) by human:sam", Kind: "change", Layer: "pin"}}
	if harm, _, _ := gate(im, objs, "b"); harm == "" {
		t.Fatal("confounded, but harm seen: want harm, so the first stage is undone")
	}
	im.After["GET /k p99"] = 90
	if _, unjudged, evidence := gate(im, objs, "b"); unjudged == "" || !strings.Contains(evidence, "100 → 90") {
		t.Fatalf("confounded, no harm: unjudged %q, evidence %q; want it unjudged, with what it saw", unjudged, evidence)
	}
}

// What every member shares is said too: a comparison alone says nothing when
// the whole fleet changed the same way, as in a squeeze on every replica.
func TestTheFleetSaysWhatEveryMemberShares(t *testing.T) {
	ms := []FleetMember{{Member: Member{Name: "a"}}, {Member: Member{Name: "b"}}, {Member: Member{Name: "c"}}}
	sums := make([]*peerSummary, 3)
	for i := range sums {
		sums[i] = &peerSummary{
			Pressures: []Pressure{{Resource: CPU, Why: "throttled"}},
			Env:       []envChange{{Event: "cpu_limit_changed", Old: 2, New: 0.25, Ago: time.Duration(3+i) * time.Minute}},
			Held:      []Held{{Knob: "compress_level", Value: "1", By: "agent:oncall", EndsIn: time.Duration(20+i) * time.Minute}},
		}
	}
	got := strings.Join(common(ms, sums), "\n")
	for _, want := range []string{"every member is short of cpu", "every member's CPU limit changed 2 → 0.25 cores", "compress_level 1 on every member", "ending in 20m to 22m"} {
		if !strings.Contains(got, want) {
			t.Errorf("common = %q, want %q", got, want)
		}
	}
	sums[1].Pressures, sums[1].Env, sums[1].Held = nil, nil, nil
	if got := common(ms, sums); len(got) != 0 {
		t.Fatalf("one member without them: common = %q, want nothing", got)
	}
}
