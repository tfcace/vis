// Command loadgen drives cacheproxy over real HTTP from outside its container,
// so squeezing the container's CPU throttles the server, not the load.
// Normal traffic follows a Zipf curve over 20,000 keys; a scan-heavy tenant
// asks for keys nobody else does. It measures latency as clients see it.
//
//	go run ./examples/cacheproxy/loadgen -target http://127.0.0.1:8790
//	curl -XPOST 127.0.0.1:8792/load -d '{"rps":800,"scan_rps":0}'
//	curl 127.0.0.1:8792/stats
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	target := flag.String("target", "http://127.0.0.1:8790", "cacheproxy base URL, or several, comma-separated, taken in turn as a load balancer would")
	control := flag.String("control", "127.0.0.1:8792", "where to listen for /load and /stats (localhost only)")
	maxInFlight := flag.Int("inflight", 256, "requests in flight to each target before new ones to it are shed: a per-backend limit without health checks, so a slow replica sheds its own share, not the healthy ones', and a dead one still gets its share")
	maxConns := flag.Int("conns", 256, "connections to each target, reused; container port forwarding (Docker Desktop, Rancher Desktop) is fragile with many")
	flag.Parse()

	g := newGen(strings.Split(*target, ","), *maxInFlight, *maxConns)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go g.run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /load", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RPS     int `json:"rps"`
			ScanRPS int `json:"scan_rps"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.rps.Store(int64(req.RPS))
		g.scanRPS.Store(int64(req.ScanRPS))
		json.NewEncoder(w).Encode(req)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(g.take())
	})
	ln, err := net.Listen("tcp", *control)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Printf("loadgen: driving %s, control on %s", strings.Join(g.targets, ", "), ln.Addr())
	srv.Serve(ln)
}

// newGen drives targets in turn, each tenant's in its own turn, with a cap on
// requests in flight to each target.
func newGen(targets []string, maxInFlight, maxConns int) *gen {
	g := &gen{targets: targets, byTarget: map[string][]time.Duration{}, shedBy: map[string]int{}, failedBy: map[string]int{}, sem: map[string]chan struct{}{}, next: map[string]*atomic.Uint64{"web": {}, "scan": {}}, client: &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: maxConns, MaxConnsPerHost: maxConns},
	}}
	for _, t := range targets {
		g.sem[t] = make(chan struct{}, maxInFlight)
	}
	return g
}

type gen struct {
	targets      []string
	next         map[string]*atomic.Uint64 // each tenant's turn: one shared turn locked a tenant onto a replica when rates divided evenly
	client       *http.Client
	sem          map[string]chan struct{} // each target's cap on requests in flight
	rps, scanRPS atomic.Int64
	scanKey      atomic.Int64

	mu        sync.Mutex
	latencies []time.Duration // since the last /stats
	byTarget  map[string][]time.Duration
	shedBy    map[string]int
	failedBy  map[string]int
	sent, ok  int
	failed    int
	limited   int
	shed      int
}

// stats is what clients saw since the last call.
type stats struct {
	Sent    int     `json:"sent"`
	OK      int     `json:"ok"`
	Failed  int     `json:"failed"`  // errors and non-200 responses, except limited
	Limited int     `json:"limited"` // 429: the service turned a tenant away on purpose (a scan over its limit)
	Shed    int     `json:"shed"`    // never sent: too many in flight
	P50     float64 `json:"p50_ms"`
	P99     float64 `json:"p99_ms"`
	// With several targets, what clients saw from each: which replica is slow.
	Targets map[string]targetStats `json:"targets,omitempty"`
}

type targetStats struct {
	OK     int     `json:"ok"`
	Failed int     `json:"failed"`
	Shed   int     `json:"shed"` // never sent: too many in flight to it
	P99    float64 `json:"p99_ms"`
}

func (g *gen) take() stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := stats{Sent: g.sent, OK: g.ok, Failed: g.failed, Limited: g.limited, Shed: g.shed}
	if n := len(g.latencies); n > 0 {
		slices.Sort(g.latencies)
		s.P50 = ms(g.latencies[n/2])
		s.P99 = ms(g.latencies[n*99/100])
	}
	if len(g.targets) > 1 {
		s.Targets = map[string]targetStats{}
		for _, t := range g.targets {
			ls := g.byTarget[t]
			ts := targetStats{OK: len(ls), Failed: g.failedBy[t], Shed: g.shedBy[t]}
			if len(ls) > 0 {
				slices.Sort(ls)
				ts.P99 = ms(ls[len(ls)*99/100])
			}
			if ts.OK > 0 || ts.Shed > 0 || ts.Failed > 0 { // one that only fails shows, failing: not as a quiet one
				s.Targets[t] = ts
			}
			g.byTarget[t], g.shedBy[t], g.failedBy[t] = ls[:0], 0, 0
		}
	}
	g.latencies, g.sent, g.ok, g.failed, g.limited, g.shed = g.latencies[:0], 0, 0, 0, 0, 0
	return s
}

func (g *gen) run(ctx context.Context) {
	zipf := rand.NewZipf(rand.New(rand.NewPCG(1, 2)), 1.1, 1, 20000)
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	var carryN, carryS float64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		carryN += float64(g.rps.Load()) / 100
		carryS += float64(g.scanRPS.Load()) / 100
		for ; carryN >= 1; carryN-- {
			g.fire(ctx, "web", "hot-"+strconv.FormatUint(zipf.Uint64(), 10))
		}
		for ; carryS >= 1; carryS-- {
			g.fire(ctx, "scan", "scan-"+strconv.FormatInt(g.scanKey.Add(1), 10))
		}
	}
}

func (g *gen) fire(ctx context.Context, tenant, key string) {
	turn := g.next[tenant]
	if turn == nil {
		turn = g.next["web"]
	}
	target := g.targets[turn.Add(1)%uint64(len(g.targets))]
	sem := g.sem[target]
	select {
	case sem <- struct{}{}:
	default: // this target is full: its request is shed, the others' aren't
		g.count(func() { g.shed++; g.shedBy[target]++ })
		return
	}
	go func() {
		defer func() { <-sem }()
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target+"/k/"+key, nil)
		req.Header.Set("X-Tenant", tenant)
		res, err := g.client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		d := time.Since(start)
		g.count(func() {
			g.sent++
			if err == nil && res.StatusCode == http.StatusTooManyRequests {
				g.limited++
				return
			}
			if err != nil || res.StatusCode != http.StatusOK {
				g.failed++
				g.failedBy[target]++
				return
			}
			g.ok++
			if len(g.latencies) < 100000 {
				g.latencies = append(g.latencies, d)
				if len(g.targets) > 1 {
					g.byTarget[target] = append(g.byTarget[target], d)
				}
			}
		})
	}()
}

func (g *gen) count(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
