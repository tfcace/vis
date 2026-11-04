package vis

import (
	"math/bits"
	"time"
)

// hist is a log-linear latency histogram: each power of two is split into 8
// buckets, so any reported quantile is within about 1/8 of the true value.
// Histograms merge by adding counts, which is what lets percentiles be
// computed after merging (per window here, per fleet later) instead of
// averaging percentiles, which gives wrong answers.
type hist struct {
	counts [histBuckets]uint32
	n      uint64
}

const (
	subBits     = 3
	subBuckets  = 1 << subBits
	maxExp      = 40 // durations are capped at 2^40 ns, about 18 minutes
	histBuckets = subBuckets + (maxExp-subBits)*subBuckets
)

func bucketOf(d time.Duration) int {
	v := uint64(max(d, 0))
	if v >= 1<<maxExp {
		v = 1<<maxExp - 1
	}
	if v < subBuckets {
		return int(v)
	}
	e := bits.Len64(v) - 1
	m := (v >> (e - subBits)) & (subBuckets - 1)
	return subBuckets + (e-subBits)*subBuckets + int(m)
}

// valueOf is the middle of bucket i.
func valueOf(i int) time.Duration {
	if i < subBuckets {
		return time.Duration(i)
	}
	e := (i-subBuckets)/subBuckets + subBits
	m := uint64((i - subBuckets) % subBuckets)
	lo := (subBuckets + m) << (e - subBits)
	width := uint64(1) << (e - subBits)
	return time.Duration(lo + width/2)
}

func (h *hist) add(d time.Duration) {
	h.counts[bucketOf(d)]++
	h.n++
}

func (h *hist) merge(o *hist) {
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.n += o.n
}

// quantile returns the value below which a fraction q of observations fall.
func (h *hist) quantile(q float64) time.Duration {
	if h.n == 0 {
		return 0
	}
	rank := uint64(q*float64(h.n-1)) + 1
	var seen uint64
	for i, c := range h.counts {
		seen += uint64(c)
		if seen >= rank {
			return valueOf(i)
		}
	}
	return valueOf(histBuckets - 1)
}
