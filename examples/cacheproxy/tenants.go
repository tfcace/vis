package main

import (
	"sync"
	"time"

	"github.com/tfcace/vis"
)

// tenants counts requests per tenant (the X-Tenant header) over the last ten
// seconds, publishes each tenant's rate as a vis signal, and admits the scan
// tenant up to ScanLimit requests per second.
type tenants struct {
	mu      sync.Mutex
	in      *vis.Instance
	byName  map[string]*rate
	scanned rate // admitted scan requests, for the limit
	reject  rate
}

const maxTenants = 32 // tenant names come from a header: user input

// rate counts events in one-second slots over the last ten seconds.
type rate struct {
	slots [10]int
	at    [10]int64 // which second each slot holds
}

func (r *rate) add(now time.Time) {
	s := now.Unix()
	i := s % 10
	if r.at[i] != s {
		r.at[i], r.slots[i] = s, 0
	}
	r.slots[i]++
}

// inLast is the count over the last n whole seconds, including the current one.
func (r *rate) inLast(now time.Time, n int64) int {
	s, total := now.Unix(), 0
	for i := range r.slots {
		if s-r.at[i] < n {
			total += r.slots[i]
		}
	}
	return total
}

func (r *rate) perSecond(now time.Time) float64 { return float64(r.inLast(now, 10)) / 10 }

func newTenants(in *vis.Instance) *tenants {
	t := &tenants{in: in, byName: map[string]*rate{}}
	in.Signal("rejected_rps", "rps", "Requests per second refused by scan_limit, over the last 10 s.", func() float64 {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.reject.perSecond(time.Now())
	})
	return t
}

// admit counts a request and says whether to serve it. Only the scan tenant is limited.
func (t *tenants) admit(name string, limit int) bool {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tenant(name).add(now)
	if name != "scan" {
		return true
	}
	if t.scanned.inLast(now, 1) >= limit {
		t.reject.add(now)
		return false
	}
	t.scanned.add(now)
	return true
}

func (t *tenants) tenant(name string) *rate {
	if r := t.byName[name]; r != nil {
		return r
	}
	if len(t.byName) >= maxTenants {
		name = "other"
		if r := t.byName[name]; r != nil {
			return r
		}
	}
	r := &rate{}
	t.byName[name] = r
	t.in.Signal("tenant_rps."+name, "rps", "Requests per second from tenant "+name+" (X-Tenant), over the last 10 s, admitted or not.", func() float64 {
		t.mu.Lock()
		defer t.mu.Unlock()
		return r.perSecond(time.Now())
	})
	return r
}
