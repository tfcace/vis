package vis_test

// These tests came from an eighth review of the fleet layer, each a reproducer of a defect it found.

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// No load: the first stage can't be judged and is kept. Its why says how to
// take it back; following that drops the earlier fix the change replaced.
func TestAStopThatKeepsTheChangeSaysHowToTakeItAllBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		_, _ = b, c
		o := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		time.Sleep(5 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		if out, isErr := call(t, cs, "set_knob", map[string]any{"instance": o[0], "set": map[string]any{"fleet_test_knob": 3}, "for": "30m", "reason": "earlier fix"}); isErr {
			t.Fatal(out)
		}
		time.Sleep(65 * time.Second)
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "1h", "reason": "x"})
		t.Logf("outcome=%v\nwhy=%v\nundo=%v", out["outcome"], out["why"], out["undo"])
		if u := fmt.Sprint(out["undo"]); !strings.Contains(u, "then set_knob instance="+o[0]+" fleet_test_knob=3 for the time left until") || !strings.Contains(fmt.Sprint(out["why"]), "undo") {
			t.Errorf("a stop that keeps the change: why %q, undo %q; want why to point at undo, and undo to put back the earlier fix it replaced", out["why"], u)
		}
	})
}
