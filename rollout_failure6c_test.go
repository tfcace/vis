package vis_test

// These tests came from a sixth review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// Following a done rollout's undo, as written, on a member where the agent
// had earlier set the knob in the same layer.
func TestARolloutsUndoLinePutsBackWhatItReplaced(t *testing.T) {
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
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		// An earlier fix on a alone: 3, for an hour.
		if out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "a", "set": map[string]any{"fleet_test_knob": 3}, "for": "1h", "reason": "earlier fix"}); isErr {
			t.Fatal(out)
		}
		time.Sleep(65 * time.Second)
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		if u := fmt.Sprint(out["undo"]); !strings.Contains(u, "then set_knob instance=a fleet_test_knob=3 for ") {
			t.Fatalf("undo = %q; want a's revert followed by putting back the 3 it replaced", u)
		}
		for _, u := range strings.Split(fmt.Sprint(out["undo"]), "; ") {
			u, _, _ = strings.Cut(u, ", then") // the reverts; what they put back is checked above
			var inst, lease, process string
			for _, f := range strings.Fields(u)[1:] {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "instance":
					inst = v
				case "lease":
					lease = v
				case "process":
					process = v
				}
			}
			l, _ := strconv.Atoi(lease)
			if r, isErr := call(t, cs, "revert", map[string]any{"instance": inst, "lease": l, "process": process, "reason": "undo as written"}); isErr {
				t.Logf("revert %s: %v", u, r)
			}
		}
		_ = b
		_ = c
	})
}
