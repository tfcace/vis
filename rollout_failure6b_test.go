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

// Open-loop load, 100 rps per member: 200 ms before the change, a backlog
// draining at 1.5 s for 8 s after it, then 10 ms.
func TestADoneRolloutsFleetLineLeavesOutTheDrain(t *testing.T) {
	for _, off := range []time.Duration{0, 2500 * time.Millisecond, 5 * time.Second, 7500 * time.Millisecond} {
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
						var changedAt time.Time
						for ctx.Err() == nil {
							if changedAt.IsZero() && fleetKnob.Get(in.Snapshot()) == 2 {
								changedAt = time.Now()
							}
							d := 200 * time.Millisecond
							if !changedAt.IsZero() {
								d = 10 * time.Millisecond
								if time.Since(changedAt) < 8*time.Second {
									d = 1500 * time.Millisecond
								}
							}
							load.Go(func() {
								_, end := in.StartRequest(ctx)
								time.Sleep(d)
								end("GET /fleet/{k}", 200)
							})
							time.Sleep(10 * time.Millisecond)
						}
					})
				}
				time.Sleep(65*time.Second + off)
				cs := connectOver(t, n, a, "a:7070", "oncall-secret")
				out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "slow"})
				time.Sleep(time.Until(mustTime(t, out["measured_at"])) + time.Second)
				f, _ := call(t, cs, "fleet", nil)
				out, _ = f["last_rollout"].(map[string]any)
				if f := fmt.Sprint(out["fleet"]); !strings.Contains(f, "meets") {
					t.Errorf("fleet = %q; every member is at 10 ms: want it to meet its objective, not the drain's p99", f)
				}
			})
		})
	}
}
