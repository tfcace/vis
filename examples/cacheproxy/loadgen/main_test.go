package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A slow replica sheds its own requests, not the healthy ones': each replica
// has its own cap on requests in flight, as a load balancer with per-backend
// limits would. With one cap for all, the slow one's requests filled it and
// requests to healthy replicas were dropped too.
func TestASlowReplicaShedsOnlyItsOwnRequests(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	fast := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer fast.Close()

	g := newGen([]string{slow.URL, fast.URL}, 4, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 40 { // alternating: 20 to each
		g.fire(ctx, "web", "k")
		time.Sleep(time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	s := g.take()
	if f := s.Targets[fast.URL]; f.Shed != 0 || f.OK != 20 {
		t.Errorf("the fast replica: %+v; want all 20 served, none shed", f)
	}
	if sl := s.Targets[slow.URL]; sl.Shed != 16 {
		t.Errorf("the slow replica: %+v; want 4 in flight, the other 16 shed", sl)
	}
}

// A replica whose requests all fail still shows in the stats, with its
// failures: left out, the timeline read it as a healthy p99 of 0 ms.
func TestAFailingReplicaShowsItsFailures(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer bad.Close()
	g := newGen([]string{ok.URL, bad.URL}, 16, 16)
	for range 10 {
		g.fire(context.Background(), "web", "k")
	}
	time.Sleep(200 * time.Millisecond)
	if b := g.take().Targets[bad.URL]; b.Failed != 5 || b.OK != 0 {
		t.Errorf("the failing replica: %+v; want its 5 failures", b)
	}
}

// Each tenant's requests spread over every replica, whatever the rates: with
// one turn shared by the tenants, rates that divide evenly by the replicas
// sent one tenant's every request to one replica.
func TestEachTenantSpreadsOverEveryReplica(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]map[string]int{} // tenant, then target
	var urls []string
	for range 3 {
		var s *httptest.Server
		s = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			tn := r.Header.Get("X-Tenant")
			if seen[tn] == nil {
				seen[tn] = map[string]int{}
			}
			seen[tn][s.URL]++
		}))
		defer s.Close()
		urls = append(urls, s.URL)
	}
	g := newGen(urls, 64, 64)
	for range 30 { // a tick of 2 web and 1 scan, as at 200 and 100 rps
		g.fire(context.Background(), "web", "k")
		g.fire(context.Background(), "web", "k")
		g.fire(context.Background(), "scan", "s")
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for _, tn := range []string{"web", "scan"} {
		if len(seen[tn]) != 3 {
			t.Errorf("%s reached %d of 3 replicas: %v", tn, len(seen[tn]), seen[tn])
		}
	}
}
