package vis_test

// These tests came from a seventh review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

func TestTheUndoLineSaysItEndsAPin(t *testing.T) {
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
			if v <= 3 {
				return 10 * time.Millisecond
			}
			return 200 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "sam-secret")
		e, err := a.Set(vis.Change{Knob: "fleet_test_knob", Value: 3, By: sam, Reason: "pin, no end"})
		t.Logf("pin: %v %v", e, err)
		time.Sleep(65 * time.Second)
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		t.Logf("rollout isErr=%v outcome=%v why=%v\nstages=%v\nundo=%v", isErr, out["outcome"], out["why"], out["stages"], out["undo"])
		for _, st := range out["stages"].([]any) {
			for _, c := range st.(map[string]any)["changes"].([]any) {
				t.Logf("change %v replaced=%v", c.(map[string]any)["instance"], c.(map[string]any)["replaced"])
			}
		}
		if u := fmt.Sprint(out["undo"]); !strings.Contains(u, "human:sam") || !strings.Contains(u, "pin") {
			t.Errorf("undo = %q; want it to say reverting ends human:sam's pin, which had no end", u)
		}
	})
}
