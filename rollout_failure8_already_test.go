package vis_test

// These tests came from an eighth review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// The first member in order already holds 9 under a short earlier lease; the
// next, the canary, is harmed by 9. The rollout stops, "the rest were left
// alone"; yet the first still holds 9, now under the rollout's hour.
func TestHarmTakesBackWhatTheRolloutLeftOnMembersThatAlreadyHeldIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v == 9 {
				return 200 * time.Millisecond
			}
			return 10 * time.Millisecond
		}, a, b, c)
		ins := map[string]*vis.Instance{"a": a, "b": b, "c": c}
		o := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		if out, isErr := call(t, cs, "set_knob", map[string]any{"instance": o[0], "set": map[string]any{"fleet_test_knob": 9}, "for": "5m", "reason": "a short try"}); isErr {
			t.Fatal(out)
		}
		time.Sleep(65 * time.Second)
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "1h", "reason": "x"})
		t.Logf("order=%v outcome=%v\nwhy=%v\nundo=%v\nstages=%v", o, out["outcome"], out["why"], out["undo"], out["stages"])
		time.Sleep(10 * time.Minute) // the short try would have ended
		t.Logf("%s after 10 min: %d", o[0], fleetKnob.Get(ins[o[0]].Snapshot()))
		if fleetKnob.Get(ins[o[0]].Snapshot()) == 9 {
			t.Errorf("%s still holds the harmful 9 ten minutes on, under the rollout's lease, while why said %q", o[0], out["why"])
		}
	})
}
