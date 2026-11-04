package main

import (
	"hash/fnv"
	"slices"
	"strconv"
	"sync"
)

// Peers sit on a consistent-hash ring that background workers keep rebuilt,
// so a lookup always finds a current one.

const ringWorkers = 6

func (a *app) startRingRefresh() (stop func()) {
	quit := make(chan struct{})
	var wg sync.WaitGroup
	for w := range ringWorkers {
		wg.Go(func() { a.rebuildRing(w, quit) })
	}
	return func() { close(quit); wg.Wait() }
}

func (a *app) rebuildRing(w int, quit <-chan struct{}) {
	members := make([]string, 64)
	for i := range members {
		members[i] = "node-" + strconv.Itoa(i) + ".cacheproxy:8790"
	}
	for gen := w; ; gen += ringWorkers {
		select {
		case <-quit:
			return
		default:
		}
		ring := make([]uint64, 0, len(members)*32)
		for _, m := range members {
			for v := range 32 {
				h := fnv.New64a()
				h.Write([]byte(m))
				h.Write([]byte("#" + strconv.Itoa(v) + "@" + strconv.Itoa(gen)))
				ring = append(ring, h.Sum64())
			}
		}
		slices.Sort(ring)
		a.ringHead.Store(ring[0])
	}
}
