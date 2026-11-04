package vis_test

// These tests came from a seventh review of the fleet layer, each a reproducer of a defect it found.

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

// The undo line followed some time after the rollout: what was replaced
// would have ended by then, yet following the line brings it back.
func TestTheUndoLineDoesntBringBackWhatHadEnded(t *testing.T) {
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
		if out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "a", "set": map[string]any{"fleet_test_knob": 3}, "for": "10m", "reason": "earlier fix"}); isErr {
			t.Fatal(out)
		}
		time.Sleep(65 * time.Second)
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "1h", "reason": "x"})
		undo := fmt.Sprint(out["undo"])
		t.Logf("undo = %s", undo)
		time.Sleep(15 * time.Minute) // a's earlier 10 m fix would have ended by now
		for _, u := range strings.Split(undo, "; ") {
			for _, step := range strings.Split(u, ", then ") {
				step, _, _ = strings.Cut(step, " (")
				var until time.Time
				if s, ts, ok := strings.Cut(step, " for the time left until "); ok { // as the line says: what is left by then, or skip it
					until, _ = time.Parse(time.RFC3339, ts)
					step = s
					if time.Until(until) <= 0 {
						continue
					}
				}
				f := strings.Fields(step)
				args := map[string]any{"reason": "undo as written"}
				if !until.IsZero() {
					args["for"] = time.Until(until).Round(time.Second).String()
				}
				set := map[string]any{}
				for i := 1; i < len(f); i++ {
					k, v, _ := strings.Cut(f[i], "=")
					switch {
					case k == "instance":
						args[k] = v
					case k == "lease":
						l, _ := strconv.Atoi(v)
						args[k] = l
					case k == "process":
						args[k] = v
					case f[i] == "for":
						args["for"] = f[i+1]
						i++
					default:
						x, _ := strconv.Atoi(v)
						set[k] = x
					}
				}
				if f[0] == "set_knob" {
					args["set"] = set
				}
				r, isErr := call(t, cs, f[0], args)
				t.Logf("%s %v -> isErr=%v %v", f[0], args, isErr, r["effects"])
			}
		}
		if v := fleetKnob.Get(a.Snapshot()); v != 5 {
			t.Errorf("a after following the undo 15 min later: fleet_test_knob=%d; without the rollout it would be back at its default 5 (the earlier 10 m fix ended)", v)
		}
	})
}
