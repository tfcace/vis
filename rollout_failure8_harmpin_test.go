package vis_test

// These tests came from an eighth review of the fleet layer, each a reproducer of a defect it found.

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

// A person's pin with no end on every member; the person's rollout harms the canary.
func TestUndoingHarmPutsBackAPinWithNoEnd(t *testing.T) {
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
		for _, in := range []*vis.Instance{a, b, c} {
			if _, err := in.Set(vis.Change{Knob: "fleet_test_knob", Value: 3, By: sam, Reason: "pin, no end"}); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "sam-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "x"})
		t.Logf("isErr=%v outcome=%v\nwhy=%v", isErr, out["outcome"], out["why"])
		for _, in := range []*vis.Instance{a, b, c} {
			t.Logf("value now: %d", fleetKnob.Get(in.Snapshot()))
		}
		if out["outcome"] == "failed" {
			t.Errorf("outcome failed, why %q, with every member's knob reverted: %s", out["why"], fmt.Sprint(fleetKnob.Get(a.Snapshot()), fleetKnob.Get(b.Snapshot()), fleetKnob.Get(c.Snapshot())))
		}
	})
}
