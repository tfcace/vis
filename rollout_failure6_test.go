package vis_test

// These tests came from a sixth review of the fleet layer, each a reproducer of a defect it found.

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

// A no-op change on a quiet route (about 1 request a second per member)
// should not read as harm.
func TestANoOpRolloutOnAQuietRouteIsntHarm(t *testing.T) {
	for _, off := range []time.Duration{0, 3 * time.Second, 6 * time.Second, 9 * time.Second} {
		t.Run(fmt.Sprint(off), func(t *testing.T) {
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
				for _, in := range []*vis.Instance{a, b, c} {
					load.Go(func() {
						for ctx.Err() == nil {
							_, end := in.StartRequest(ctx)
							time.Sleep(10 * time.Millisecond)
							end("GET /fleet/{k}", 200)
							time.Sleep(990 * time.Millisecond)
						}
					})
				}
				time.Sleep(65*time.Second + off)
				cs := connectOver(t, n, a, "a:7070", "oncall-secret")
				out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "no-op"})
				t.Logf("isErr=%v outcome=%v why=%v\nevidence=%v", isErr, out["outcome"], out["why"], out["stages"])
				// done, or too few requests to judge (a p99 of a handful is their slowest one), with the change kept; never harm
				why := fmt.Sprint(out["why"])
				if out["outcome"] != "done" && !strings.Contains(why, "too few requests") || strings.Contains(why, "got worse") || strings.Contains(why, "undone") {
					t.Errorf("a no-op change on a 1 rps route: outcome %v: %v", out["outcome"], why)
				}
			})
		})
	}
}
