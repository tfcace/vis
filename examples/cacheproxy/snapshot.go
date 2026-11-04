package main

import (
	"hash/crc32"
	"time"
)

// The cache snapshots its index every second, with a checksum of each entry,
// so a restart can tell whether the index it reloads is intact.

const snapshotEvery = time.Second

var snapshotRecord = func() []byte {
	b := make([]byte, 128<<10)
	for rest := b; len(rest) > 0; {
		rest = rest[copy(rest, payload):]
	}
	return b
}()

var snapshotTable = crc32.MakeTable(crc32.Castagnoli)

func (c *lru) startSnapshots(every time.Duration) (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, len(snapshotRecord))
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
			case <-quit:
				return
			}
			c.snapshot(buf)
		}
	}()
	return func() { close(quit); <-done }
}

func (c *lru) snapshot(buf []byte) {
	var sum uint32
	c.mu.Lock()
	for e := c.order.Front(); e != nil; e = e.Next() {
		n := copy(buf, e.Value.(string))
		copy(buf[n:], snapshotRecord)
		sum = crc32.Update(sum, snapshotTable, buf)
	}
	c.mu.Unlock()
	c.snapshotSum.Store(sum)
	c.snapshots.Add(1)
}
