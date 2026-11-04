package vis_test

// These tests came from a third review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// X1: the first member crashes on the change and restarts under its name within
// the settle time. Change and lease IDs are per process, so the gate reads the
// new process's change with the same number, and the result tells the agent to
// revert a lease that is now someone else's.
func TestAFirstMemberRestartedDuringTheGateIsNotRevertedBlind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		var stopB2 func()
		var b2 *vis.Instance
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopC()
			if stopB2 != nil {
				stopB2()
			}
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v >= 9 {
				return 300 * time.Millisecond
			}
			return 10 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second)
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		if first != "b" {
			t.Skipf("first is %s", first)
		}
		go func() {
			time.Sleep(5 * time.Second) // the change crashes b
			vis.SkipLeave(b)
			stopB()
			time.Sleep(time.Second)
			b2, stopB2 = peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
			// at start, a person holds a value on the new process: its change 1, lease 1
			if _, err := b2.Set(vis.Change{Knob: "fleet_test_knob", Value: 7, By: vis.Writer{Kind: vis.Agent, ID: "watcher"}, For: 10 * time.Minute, Reason: "start-up hold by another agent"}); err != nil {
				t.Error(err)
			}
		}()
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"})
		t.Logf("isErr=%v outcome=%v\nwhy=%v\nstages=%v", isErr, out["outcome"], out["why"], out["stages"])
		time.Sleep(5 * time.Second) // the new b is up, holding sam's start-up value
		if b2 == nil {
			t.Fatalf("no new b: %v", out)
		}
		// the agent does as told: "to take it back, revert with instance=b lease=1"
		rv, rvErr := call(t, cs, "revert", map[string]any{"instance": "b", "lease": 1, "reason": "take the rollout back, as the result said"})
		t.Logf("revert as told: isErr=%v %v", rvErr, rv)
		st, _ := b2.Stack("fleet_test_knob")
		t.Logf("new b holds %d, top layer by %v (lease %d)", fleetKnob.Get(b2.Snapshot()), st[0].By, st[0].Lease)
		why := fmt.Sprint(out["why"])
		if fleetKnob.Get(b2.Snapshot()) != 9 && (contains(why, "keeps the change") || contains(why, "undone on")) {
			t.Errorf("the result says b keeps (or had undone) the rollout's change, but the restart dropped it: %s", why)
		}
		time.Sleep(time.Minute) // the old b's in-flight handlers end
	})
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}

// X2: a member stage 1 skips as masked still logs a change (5 → 5), which the
// first member's comparison then names as the rest's own change.
func TestAMaskedWriteIsNotTheRestChanging(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, slow9)
		defer stop()
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		time.Sleep(3 * time.Second)
		if _, err := ins[order[0]].Set(vis.Change{Knob: "fleet_test_knob", Value: 5, By: sam, Reason: "pinned at 5"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 4}, "for": "10m", "reason": "try 4"})
		st := out["stages"].([]any)[0].(map[string]any)
		ch := st["changes"].([]any)
		last := ch[len(ch)-1].(map[string]any)
		im, _ := call(t, cs, "impact", map[string]any{"instance": last["instance"], "change": last["change"]})
		t.Logf("order=%v masked=%v first=%v outcome=%v\nimpact summary: %v", order, st["masked"], last["instance"], out["outcome"], im["summary"])
		if s := fmt.Sprint(im["summary"]); contains(s, "changed too") {
			t.Errorf("the rollout's own masked write reads as the rest changed: %s", s)
		}
	})
}
