package vis

// These tests came from a second review of the fleet layer, each a reproducer of a defect it found.

import (
	"testing"
	"testing/synctest"
	"time"
)

// A sub-millisecond service: p99 more than doubles, under the objective, and the gate passes it.
func TestTheGateSeesSubMillisecondHarm(t *testing.T) {
	objs := []sloDef{{"GET /k", 5 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /k p99": 0.4}, After: map[string]float64{"GET /k p99": 0.95},
		Rest: &Rest{Members: []string{"a", "c"}, Before: map[string]float64{"GET /k p99": 0.4}, After: map[string]float64{"GET /k p99": 0.4}}}
	if harm, _, ev := gate(im, objs, "b"); harm == "" {
		t.Errorf("p99 0.4 → 0.95 ms (2.4x) while the rest held passes: %s", ev)
	}
}

// Two objectives: the second route goes 10 → 400 ms, over its objective, and the gate passes it.
func TestTheGateJudgesEveryObjective(t *testing.T) {
	objs := []sloDef{{"GET /a", 50 * time.Millisecond}, {"GET /b", 50 * time.Millisecond}}
	im := Impact{Before: map[string]float64{"GET /a p99": 10, "GET /b p99": 10}, After: map[string]float64{"GET /a p99": 10, "GET /b p99": 400}}
	if harm, _, ev := gate(im, objs, "b"); harm == "" {
		t.Errorf("GET /b 10 → 400 ms against 50 ms passes: %s", ev)
	}
}

// Partitioned: a's person unfreezes, freezes, unfreezes; afterwards an agent on b
// (still frozen from before) freezes again. The freeze set last is lost on heal.
func TestARefreezeSetLastWinsAfterAPartition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := NewInstance(InstanceName("a")), NewInstance(InstanceName("b"))
		agent, sam := Writer{Kind: Agent, ID: "watcher"}, Writer{Kind: Human, ID: "sam"}
		a.Freeze(agent, "first")
		b.adoptFreeze(a.FreezeState())
		time.Sleep(time.Second)
		a.Unfreeze(sam, "go")
		time.Sleep(time.Second)
		a.Freeze(sam, "wait")
		time.Sleep(time.Second)
		a.Unfreeze(sam, "go again")
		time.Sleep(10 * time.Second)
		b.Freeze(agent, "p99 is up: hold everything") // set last of all
		for range 3 {
			a.adoptFreeze(b.FreezeState())
			b.adoptFreeze(a.FreezeState())
		}
		t.Logf("a %+v\nb %+v", a.FreezeState(), b.FreezeState())
		if !a.FreezeState().Frozen || !b.FreezeState().Frozen {
			t.Errorf("the freeze set last, on b, was lost when the partition healed")
		}
	})
}
