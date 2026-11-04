package main

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// f4's cost model for the origin: its cost per batch degrades over 10 s
// (superlinear in the batch's size, like a query plan flip), it stalls for 2 s
// at 12 s, then for 0.4 s every 40 s from 40 s on. Not in the agent's copy.

func (o *origin) useCostModel() (restore func()) {
	t0 := time.Now()
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(4, 4))
	h := func(keys int, _ time.Duration) time.Duration {
		mu.Lock()
		z := rng.NormFloat64()
		mu.Unlock()
		return batchCost(o, time.Since(t0), keys, z)
	}
	o.hook.Store(&h)
	return func() { o.hook.Store(nil) }
}

// batchCost is the origin's time for a batch of keys started el into the
// model, with z drawn from a standard normal.
func batchCost(o *origin, el time.Duration, keys int, z float64) time.Duration {
	r := min(1, float64(el)/float64(10*time.Second))
	base := o.base + time.Duration(r*float64(4*time.Millisecond))
	sq := o.perKeySq + time.Duration(r*float64(time.Millisecond-o.perKeySq))
	n := time.Duration(keys)
	d := time.Duration(float64(base+n*o.perKey+n*n*sq) * math.Exp(0.25*z))
	return d + batchHold(el)
}

// batchHold is how long a batch started el into the model waits first.
func batchHold(el time.Duration) time.Duration {
	if el >= 12*time.Second && el < 14*time.Second {
		return 14*time.Second - el
	}
	if el >= 40*time.Second {
		p := (el - 40*time.Second) % (40 * time.Second)
		if p < 400*time.Millisecond {
			return 400*time.Millisecond - p
		}
	}
	return 0
}
