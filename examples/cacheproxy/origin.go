package main

import (
	"sync/atomic"
	"time"
)

// The origin isn't cacheproxy's code: it stands in for the backend another team
// runs, so the eval gives agents cacheproxy's source without this file.

// origin is the slow backend. A batch costs a round trip, a little per key, and
// a little more per key the bigger the batch, the way a large IN query degrades.
// So mid-size batches move the most keys per second: under load, a big
// BatchSize overloads the origin and a smaller one recovers. With a purely
// linear cost the RFC's reflex (lower BatchSize when the scheduler lags) would
// make overload worse, since each key would pay more of the fixed round trip.
type origin struct {
	base, perKey, perKeySq time.Duration
	hook                   atomic.Pointer[func(keys int, d time.Duration) time.Duration] // an experiment's cost model; nil means the plain one
}

func newOrigin() *origin {
	return &origin{base: 10 * time.Millisecond, perKey: 500 * time.Microsecond, perKeySq: 20 * time.Microsecond}
}

func (o *origin) fetch(keys int) {
	n := time.Duration(keys)
	d := o.base + n*o.perKey + n*n*o.perKeySq
	if h := o.hook.Load(); h != nil {
		d = (*h)(keys, d)
	}
	time.Sleep(d)
}
