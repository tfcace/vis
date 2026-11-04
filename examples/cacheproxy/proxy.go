package main

import (
	"container/list"
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tfcace/vis"
)

// serveKey is the request path: a cache lookup, on a miss a wait for the
// batcher to fetch the key from the origin, then compressing the response at
// the level the request's snapshot says. BatchSize is read by the batcher, and
// CacheSize reaches the cache through Bind.
//
// Clients are promised 50 ms at p99, but vis times the handler, and clients
// also wait on the connection and their own scheduling: about 10 ms more under
// load on OrbStack. So the server answers to 40 ms, leaving that room.
//
// vis:slo p99=40ms
func (a *app) serveKey(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tenant := r.Header.Get("X-Tenant")
	if tenant == "" {
		tenant = "unknown"
	}
	if !a.ten.admit(tenant, ScanLimit.From(r.Context())) {
		http.Error(w, "scan tenant over its limit", http.StatusTooManyRequests)
		return
	}
	done := vis.Probe(r.Context(), "cache")
	hit := a.cache.get(key)
	done()
	a.watchClient(r.Context(), hit)
	if !hit {
		done = vis.Probe(r.Context(), "origin")
		err := a.batch.fetch(r.Context())
		done()
		if err != nil {
			http.Error(w, err.Error(), http.StatusGatewayTimeout)
			return
		}
		done = vis.Probe(r.Context(), "cache") // the add counts with the lookup
		a.cache.add(key)
		done()
	}
	a.stats.lookup(hit) // vis times the request; the hit rate is ours
	w.Header().Set("Content-Encoding", "deflate")
	done = vis.Probe(r.Context(), "compress")
	body := encode(key, CompressLevel.From(r.Context()))
	done()
	a.stats.response(len(body))
	w.Write(body)
}

// batcher groups misses into origin fetches of up to BatchSize keys. A bigger
// batch saves round trips but makes every key in it wait for the whole batch.
type batcher struct {
	in     *vis.Instance
	origin *origin
	stats  *stats
	queue  chan *miss
}

type miss struct {
	queued time.Time
	done   chan struct{}
}

func newBatcher(in *vis.Instance, o *origin, s *stats) *batcher {
	return &batcher{in: in, origin: o, stats: s, queue: make(chan *miss, 4096)}
}

func (b *batcher) fetch(ctx context.Context) error {
	m := &miss{queued: time.Now(), done: make(chan struct{})}
	select {
	case b.queue <- m:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run keeps two fetchers busy. Each takes up to BatchSize queued misses, waiting
// at most 2 ms to fill a batch. Batch wait is how long a miss waits for its batch to start.
func (b *batcher) run(ctx context.Context) {
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for {
				var first *miss
				select {
				case first = <-b.queue:
				case <-ctx.Done():
					return
				}
				max := BatchSize.Get(b.in.Snapshot())
				batch := []*miss{first}
				window := time.After(2 * time.Millisecond)
			fill:
				for len(batch) < max {
					select {
					case m := <-b.queue:
						batch = append(batch, m)
					case <-window:
						break fill
					}
				}
				startAt := time.Now()
				for _, m := range batch {
					b.stats.wait(startAt.Sub(m.queued))
				}
				b.origin.fetch(len(batch))
				b.stats.fetched(len(batch))
				for _, m := range batch {
					close(m.done)
				}
			}
		})
	}
	wg.Wait()
}

// lru is a plain LRU of keys. Values are simulated, so only membership
// matters, unless real is set (-real-values): then each entry holds valueBytes
// of memory, its share of cache_size, and the knob's memory is real.
type lru struct {
	mu    sync.Mutex
	cap   int
	order *list.List
	items map[string]*list.Element
	real  bool
	vals  map[string][]byte // with real: each entry's value

	snapshots   atomic.Int64 // see snapshot.go
	snapshotSum atomic.Uint32
}

func newLRU(capacity int) *lru {
	return &lru{cap: capacity, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return true
	}
	return false
}

func (c *lru) add(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(k)
	if c.real {
		if c.vals == nil {
			c.vals = map[string][]byte{}
		}
		c.vals[k] = newValue()
	}
	c.evict()
}

// valueBytes is one entry's value with -real-values: a MiB of cache_size is
// entriesFor(1) entries.
const valueBytes = 128 << 10

// newValue is a value whose every page is written, so it is resident memory,
// not a mapping the kernel hasn't backed yet.
func newValue() []byte {
	v := make([]byte, valueBytes)
	for i := 0; i < len(v); i += 4096 {
		v[i] = 1
	}
	return v
}

// heldBytes is the memory the values hold: 0 without real values.
func (c *lru) heldBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.vals {
		n += len(v)
	}
	return n
}

// resize is what CacheSize's Bind calls: state built from a knob follows it.
func (c *lru) resize(capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cap = capacity
	c.evict()
}

func (c *lru) evict() {
	for c.order.Len() > c.cap {
		e := c.order.Back()
		delete(c.items, e.Value.(string))
		delete(c.vals, e.Value.(string))
		c.order.Remove(e)
	}
}

func (c *lru) size() (n, capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len(), c.cap
}
