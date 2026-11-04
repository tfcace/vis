package vis

import (
	"testing"
	"time"
)

// Every duration lands in a bucket whose middle is within 1/8 of it.
func TestHistogramResolution(t *testing.T) {
	for d := time.Duration(1); d < 1<<maxExp; d = d*9/8 + 1 {
		got := valueOf(bucketOf(d))
		if diff := got - d; diff < -d/8-1 || diff > d/8+1 {
			t.Fatalf("%v reads back as %v", d, got)
		}
	}
}

func TestHistogramQuantiles(t *testing.T) {
	var h hist
	for i := range 1000 {
		h.add(time.Duration(i+1) * time.Millisecond) // 1ms .. 1000ms
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{.5, 500 * time.Millisecond}, {.99, 990 * time.Millisecond}} {
		if got := h.quantile(c.q); got < c.want*7/8 || got > c.want*9/8 {
			t.Errorf("p%v = %v, want about %v", c.q*100, got, c.want)
		}
	}
}
