package vis_test

// These tests came from an eighth review of the fleet layer, each a reproducer of a defect it found.

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// set_knob, under a person's token, with for=0s: a pin with no end.
func TestAPersonCanPinWithNoEndOverMCP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		defer func() { stopA(); synctest.Wait() }()
		time.Sleep(5 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "sam-secret")
		out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "a", "set": map[string]any{"fleet_test_knob": 3}, "for": "0s", "reason": "pin again"})
		t.Logf("isErr=%v out=%v", isErr, out)
		time.Sleep(3 * time.Hour)
		if v := fleetKnob.Get(a.Snapshot()); v != 3 {
			t.Errorf("after 3h: %d", v)
		}
		t.Logf("after 3h: %d", fleetKnob.Get(a.Snapshot()))
	})
}
