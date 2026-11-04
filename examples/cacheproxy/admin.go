package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"sync"
)

// admin is the -faults listener: named faults an experiment can start and
// stop, a plain HTTP server outside vis. The faults live beside the code they
// imitate, under that code's names (the cache's snapshot, the hash ring, the
// client watcher, the origin's cost), so a profile of a faulty run reads like a
// real service's and doesn't name the fault. the eval's sealed key says which is which.
type admin struct {
	a    *app
	mu   sync.Mutex
	stop map[string]func() // the active faults, by name
}

var faultNames = []string{"f1", "f2", "f3", "f4", "f5"}

func newAdmin(a *app) *admin { return &admin{a: a, stop: map[string]func(){}} }

// serve listens on addr until ctx is done.
func (ad *admin) serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: ad.handler()}
	go func() { <-ctx.Done(); srv.Close() }()
	go srv.Serve(ln)
	return nil
}

func (ad *admin) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /faults", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(faultNames)
	})
	mux.HandleFunc("POST /faults/{name}/{op}", func(w http.ResponseWriter, r *http.Request) {
		name, op := r.PathValue("name"), r.PathValue("op")
		if !slices.Contains(faultNames, name) {
			http.Error(w, "no such fault", http.StatusNotFound)
			return
		}
		var active bool
		switch op {
		case "start":
			active = ad.start(name)
		case "stop":
			active = ad.end(name)
		default:
			http.Error(w, "start or stop", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": name, "active": active})
	})
	return mux
}

// start starts a fault; starting an active one does nothing.
func (ad *admin) start(name string) bool {
	ad.mu.Lock()
	defer ad.mu.Unlock()
	if ad.stop[name] != nil {
		return true
	}
	switch name {
	case "f1":
		ad.stop[name] = ad.a.cache.startSnapshots(snapshotEvery)
	case "f2":
		ad.stop[name] = ad.a.startRingRefresh()
	case "f3":
		ad.stop[name] = ad.a.startClientWatch()
	case "f4":
		ad.stop[name] = ad.a.origin.useCostModel()
	case "f5":
		ad.stop[name] = ad.a.stats.startResponseAudit()
	}
	return true
}

// end stops a fault; stopping an inactive one does nothing.
func (ad *admin) end(name string) bool {
	ad.mu.Lock()
	defer ad.mu.Unlock()
	if s := ad.stop[name]; s != nil {
		s()
		delete(ad.stop, name)
	}
	return false
}

func (ad *admin) active(name string) bool {
	ad.mu.Lock()
	defer ad.mu.Unlock()
	return ad.stop[name] != nil
}

// close stops every fault and ends f3's leftover goroutines. Tests only.
func (ad *admin) close() {
	for _, name := range faultNames {
		ad.end(name)
	}
	ad.a.endWatchers()
}
