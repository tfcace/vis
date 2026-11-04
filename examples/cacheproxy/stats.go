package main

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tfcace/vis"
)

// stats holds what only the app knows: the cache hit rate and how long misses
// wait for a batch. Request latency and the runtime's scheduler latency are vis's.
type stats struct {
	mu      sync.Mutex
	lookups int
	hits    int
	waits   []time.Duration // this bucket's batch waits
	keys    int             // this bucket's keys fetched from the origin
	bytes   int             // this bucket's response bytes
	sent    int             // and responses
	buckets []bucket        // oldest first, at most an hour

	auditOn atomic.Bool
	audit   [][]byte // f5: a record of every response while auditing, never trimmed
}

// auditRecord is what the response audit keeps per response.
const auditRecord = 1 << 10

// startResponseAudit keeps a record of every response for a compliance audit
// trail, in memory, and nothing ever trims it: what was kept stays after it
// stops.
func (s *stats) startResponseAudit() (stop func()) {
	s.auditOn.Store(true)
	return func() { s.auditOn.Store(false) }
}

// audited is how many bytes the audit trail holds.
func (s *stats) audited() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.audit) * auditRecord
}

type bucket struct {
	At         time.Time `json:"at"`
	WaitP99    float64   `json:"batch_wait_p99_ms"`
	OriginKPS  float64   `json:"origin_keys_per_s"`
	ResponseKB float64   `json:"response_kb"`
	HitRate    float64   `json:"hit_rate"`
}

const bucketEvery = 10 * time.Second

func newStats() *stats { return &stats{} }

func (s *stats) lookup(hit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups++
	if hit {
		s.hits++
	}
}

// wait records how long a miss waited for its batch to start.
func (s *stats) wait(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.waits) < 50000 {
		s.waits = append(s.waits, d)
	}
}

func (s *stats) run(ctx context.Context) {
	t := time.NewTicker(bucketEvery)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			s.rotate(now)
		case <-ctx.Done():
			return
		}
	}
}

// What only the proxy knows, published every 10 s when a bucket closes.
var (
	// vis:signal
	HitRate vis.Gauge // Share of lookups served from the cache, over the last closed 10 s bucket.

	// vis:signal name=batch_wait_p99
	BatchWait vis.Latency // How long cache misses waited for their origin batch to start, p99 over the last closed 10 s bucket.

	// vis:signal unit=KiB
	ResponseKB vis.Gauge // Average compressed response size, over the last closed 10 s bucket.

	// vis:signal name=origin_keys_per_s unit=keys/s
	OriginKeys vis.Gauge // Keys fetched from the origin per second, over the last closed 10 s bucket.
)

func (s *stats) rotate(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := bucket{At: now, WaitP99: ms(p99(s.waits)), OriginKPS: float64(s.keys) / bucketEvery.Seconds()}
	if s.sent > 0 {
		b.ResponseKB = float64(s.bytes) / float64(s.sent) / 1024
	}
	if s.lookups > 0 {
		b.HitRate = float64(s.hits) / float64(s.lookups)
	}
	HitRate.Set(b.HitRate)
	BatchWait.Set(time.Duration(b.WaitP99 * float64(time.Millisecond)))
	ResponseKB.Set(b.ResponseKB)
	OriginKeys.Set(b.OriginKPS)
	s.buckets = append(s.buckets, b)
	if len(s.buckets) > 360 {
		s.buckets = s.buckets[1:]
	}
	s.waits, s.hits, s.lookups, s.keys, s.bytes, s.sent = s.waits[:0], 0, 0, 0, 0, 0
}

func (s *stats) response(n int) {
	if s.auditOn.Load() {
		rec := make([]byte, auditRecord)
		for i := range rec {
			rec[i] = byte(n + i)
		}
		s.mu.Lock()
		s.audit = append(s.audit, rec)
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes += n
	s.sent++
}

func (s *stats) fetched(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys += n
}

// last is the newest closed bucket, or zeros before the first one closes.
func (s *stats) snapshot() []bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.buckets)
}

func p99(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	c := slices.Clone(ds)
	slices.Sort(c)
	return c[len(c)*99/100]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
