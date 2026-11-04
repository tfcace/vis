package vis_test

import (
	"testing"

	"github.com/tfcace/vis"
)

func TestAppsPublishSignals(t *testing.T) {
	in := vis.NewInstance()
	hits := 0.0
	in.Signal("hit_rate", "", "Share of requests served from the cache.", func() float64 { return hits })
	in.Signal("batch_wait_p99", "ms", "How long misses wait for a batch to start.", func() float64 { return 7.2 })
	hits = 0.83
	hit, _ := signalNamed(in, "hit_rate")
	wait, _ := signalNamed(in, "batch_wait_p99")
	if hit.Value != 0.83 || hit.About == "" || wait.Unit != "ms" || wait.Value != 7.2 {
		t.Fatalf("signals = %+v", in.Signals())
	}
}

func TestASignalThatPanicsIsReportedNotFatal(t *testing.T) {
	in := vis.NewInstance()
	in.Signal("broken", "", "", func() float64 { panic("boom") })
	if s, ok := signalNamed(in, "broken"); !ok || s.Err == "" {
		t.Fatalf("signals = %+v, want the panic reported as an error", in.Signals())
	}
}
