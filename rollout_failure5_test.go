package vis_test

// These tests came from a fifth review of the fleet layer, each a reproducer of a defect it found.

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// F1a: every member already holds the value under the agent's own earlier
// lease (it wants to extend it). The rollout stops ("takes effect nowhere")
// and its undo of the "already held" leases clears the agent layer: the fix
// falls back to the default on every member.
func TestExtendingAFixWithARolloutKeepsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		for _, m := range []string{"a", "b"} {
			if out, isErr := call(t, cs, "set_knob", map[string]any{"instance": m, "set": map[string]any{"fleet_test_knob": 2}, "for": "30m", "reason": "the fix"}); isErr {
				t.Fatalf("set_knob: %v", out)
			}
		}
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "2h", "reason": "hold the fix longer"})
		t.Logf("isErr=%v out=%v", isErr, out)
		if ga, gb := fleetKnob.Get(a.Snapshot()), fleetKnob.Get(b.Snapshot()); ga != 2 || gb != 2 {
			t.Errorf("a=%d b=%d after a rollout of the value they already held: the fix was removed", ga, gb)
		}
	})
}

// F1b: the first member in stage order already holds the value under an
// earlier lease; the next takes the change and can't be judged (no load).
// The rollout keeps the measured member's change but undoes the first's
// earlier fix.
func TestAStoppedRolloutKeepsAnEarlierFixElsewhere(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		ins := map[string]*vis.Instance{"a": a, "b": b, "c": c}
		o := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		if out, isErr := call(t, cs, "set_knob", map[string]any{"instance": o[0], "set": map[string]any{"fleet_test_knob": 2}, "for": "30m", "reason": "the fix"}); isErr {
			t.Fatalf("set_knob: %v", out)
		}
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "2h", "reason": "the fix everywhere"})
		t.Logf("order=%v isErr=%v out=%v", o, isErr, out)
		for _, m := range o {
			t.Logf("%s=%d", m, fleetKnob.Get(ins[m].Snapshot()))
		}
		if g := fleetKnob.Get(ins[o[0]].Snapshot()); g != 2 {
			t.Errorf("%s=%d: its earlier fix was undone by a rollout of the same value (%s)", o[0], g, fmt.Sprint(out["why"]))
		}
	})
}

// A masked write still takes the place of what the caller's layer held under
// the mask: when the rollout takes it back, that value comes back too, for
// the time it had left, rather than the layer going empty.
func TestUndoingAMaskedWritePutsBackWhatItReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		for _, in := range []*vis.Instance{a, b} {
			if _, err := in.Set(vis.Change{Knob: "fleet_test_knob", Value: 7, By: sam, Reason: "pinned"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 3, By: claude, For: 20 * time.Minute, Reason: "an earlier fix, under the pin"}); err != nil {
			t.Fatal(err)
		}
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "masked everywhere"})
		if out["outcome"] != "stopped" {
			t.Fatalf("rollout = %v; want stopped, masked everywhere", out)
		}
		if _, err := b.Clear("fleet_test_knob", vis.PinLayer, sam, "unpin"); err != nil {
			t.Fatal(err)
		}
		if got := fleetKnob.Get(b.Snapshot()); got != 3 {
			t.Fatalf("b = %d after the rollout was taken back and the pin lifted; want the earlier fix, 3", got)
		}
	})
}
