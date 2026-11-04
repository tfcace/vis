package vis

import (
	"math"
	"runtime/metrics"
	"testing"
	"time"
)

// Scheduler latency is a cumulative histogram; a window's percentiles come from the difference of two samples.
func TestWindowedQuantileFromCumulativeHistograms(t *testing.T) {
	buckets := []float64{math.Inf(-1), 0, 1e-6, 1e-5, 1e-4, 1e-3, 1e-2, math.Inf(1)}
	older := &metrics.Float64Histogram{Buckets: buckets, Counts: []uint64{0, 500, 400, 100, 0, 0, 0}}
	newer := &metrics.Float64Histogram{Buckets: buckets, Counts: []uint64{0, 520, 400, 100, 0, 170, 10}}
	// In the window: 20 samples at 0..1µs, 170 at 1..10ms, 10 at 10ms and up.
	if got := windowQuantile(older, newer, .5); got != 5*time.Millisecond+500*time.Microsecond {
		t.Fatalf("p50 = %v, want the middle of 1..10ms", got)
	}
	if got := windowQuantile(older, newer, .99); got != 10*time.Millisecond {
		t.Fatalf("p99 = %v, want the open bucket's lower edge, 10ms", got)
	}
	if got := windowQuantile(newer, newer, .99); got != 0 {
		t.Fatalf("empty window p99 = %v, want 0", got)
	}
}
