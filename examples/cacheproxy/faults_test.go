package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

// startFaulty runs the proxy route and the -faults handler, with no fault active.
func startFaulty(t *testing.T) (a *app, ad *admin, proxy, fx *httptest.Server) {
	t.Helper()
	a = newApp(vis.NewInstance(vis.WithLogger(slog.New(slog.DiscardHandler)), vis.Service("cacheproxy"), vis.SLO(keyRoute, 50*time.Millisecond)))
	ad = newAdmin(a)
	ctx, cancel := context.WithCancel(context.Background())
	go a.batch.run(ctx)
	proxy, fx = httptest.NewServer(a.routes()), httptest.NewServer(ad.handler())
	t.Cleanup(func() { fx.Close(); proxy.Close(); ad.close(); cancel() })
	return a, ad, proxy, fx
}

func post(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	res, err := http.Post(srv.URL+path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// eventually polls cond for up to 3 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTheFaultsListenerListsAndControlsFaults(t *testing.T) {
	_, _, _, fx := startFaulty(t)
	code, body := get(t, fx, "/faults", "")
	var names []string
	if err := json.Unmarshal(body, &names); err != nil || code != 200 || !slices.Equal(names, faultNames) {
		t.Fatalf("GET /faults: %d %s, want %v", code, body, faultNames)
	}
	if code, _ := post(t, fx, "/faults/nope/start"); code != http.StatusNotFound {
		t.Fatalf("unknown fault: %d, want 404", code)
	}
	if code, _ := post(t, fx, "/faults/f1/pause"); code != http.StatusNotFound {
		t.Fatalf("unknown op: %d, want 404", code)
	}
	for _, name := range faultNames {
		for range 2 { // starting or stopping twice is harmless
			if code, b := post(t, fx, "/faults/"+name+"/start"); code != 200 || b["active"] != true {
				t.Fatalf("start %s: %d %v", name, code, b)
			}
		}
		for range 2 {
			if code, b := post(t, fx, "/faults/"+name+"/stop"); code != 200 || b["active"] != false {
				t.Fatalf("stop %s: %d %v", name, code, b)
			}
		}
	}
}

func TestInactiveFaultsChangeNothing(t *testing.T) {
	a, _, proxy, _ := startFaulty(t)
	get(t, proxy, "/k/warm", "web")
	before := runtime.NumGoroutine()
	for range 50 {
		code, body := get(t, proxy, "/k/warm", "web")
		if code != 200 || string(body) != string(encode("warm", 9)) {
			t.Fatalf("response changed: %d, %d bytes", code, len(body))
		}
	}
	if a.origin.hook.Load() != nil || a.cache.snapshots.Load() != 0 || a.watchClients.Load() {
		t.Fatal("a fault hook is set with no fault active")
	}
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines %d -> %d with no fault active", before, after)
	}
	start := time.Now()
	a.origin.fetch(1)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("origin fetch took %v with no fault active", d)
	}
}

func TestF1StartsAndStops(t *testing.T) {
	a, ad, proxy, fx := startFaulty(t)
	get(t, proxy, "/k/a", "web")
	post(t, fx, "/faults/f1/start")
	eventually(t, "f1 to run", func() bool { return a.cache.snapshots.Load() > 0 })
	post(t, fx, "/faults/f1/stop")
	n := a.cache.snapshots.Load()
	time.Sleep(snapshotEvery + 200*time.Millisecond)
	if a.cache.snapshots.Load() != n || ad.active("f1") {
		t.Fatal("f1 still running after stop")
	}
}

func TestF2StartsAndStops(t *testing.T) {
	_, ad, _, fx := startFaulty(t)
	before := runtime.NumGoroutine()
	post(t, fx, "/faults/f2/start")
	if n := runtime.NumGoroutine(); n < before+ringWorkers {
		t.Fatalf("goroutines %d -> %d after starting f2", before, n)
	}
	post(t, fx, "/faults/f2/stop")
	eventually(t, "f2's goroutines to end", func() bool { return runtime.NumGoroutine() < before+ringWorkers })
	if ad.active("f2") {
		t.Fatal("f2 active after stop")
	}
}

func TestF3StartsAndStops(t *testing.T) {
	_, ad, proxy, fx := startFaulty(t)
	get(t, proxy, "/k/hot", "web")
	before := runtime.NumGoroutine()
	post(t, fx, "/faults/f3/start")
	for range 20 {
		get(t, proxy, "/k/hot", "web")
	}
	if n := runtime.NumGoroutine(); n < before+20 {
		t.Fatalf("goroutines %d -> %d with f3 active", before, n)
	}
	post(t, fx, "/faults/f3/stop")
	held := runtime.NumGoroutine()
	for range 20 {
		get(t, proxy, "/k/hot", "web")
	}
	if n := runtime.NumGoroutine(); n > held+5 {
		t.Fatalf("goroutines %d -> %d after f3 stopped", held, n)
	}
	ad.close()
	eventually(t, "f3's goroutines to end", func() bool { return runtime.NumGoroutine() < before+5 })
}

func TestF4StartsAndStops(t *testing.T) {
	a, _, _, fx := startFaulty(t)
	o := a.origin
	post(t, fx, "/faults/f4/start")
	if o.hook.Load() == nil {
		t.Fatal("f4 started without its hook")
	}
	normal := o.base + 64*o.perKey + 64*64*o.perKeySq
	if d := batchCost(o, 30*time.Second, 64, 0); d < 10*normal {
		t.Fatalf("f4 at 30 s: %v for 64 keys, normal %v", d, normal)
	}
	if d := batchCost(o, 0, 1, 0); d > 2*(o.base+o.perKey+o.perKeySq) {
		t.Fatalf("f4 at 0 s: %v for 1 key", d)
	}
	if batchHold(13*time.Second) == 0 || batchHold(30*time.Second) != 0 {
		t.Fatal("f4 hold schedule")
	}
	post(t, fx, "/faults/f4/stop")
	if o.hook.Load() != nil {
		t.Fatal("f4 hook still set after stop")
	}
}

// f5 keeps a record of every response and never lets go, even after stop: a
// leak that grows with traffic.
func TestF5LeaksPerResponseAndKeepsIt(t *testing.T) {
	a, ad, proxy, fx := startFaulty(t)
	get(t, proxy, "/k/a", "web")
	if a.stats.audited() != 0 {
		t.Fatal("records kept with f5 off")
	}
	post(t, fx, "/faults/f5/start")
	for range 20 {
		get(t, proxy, "/k/a", "web")
	}
	n := a.stats.audited()
	if n < 20*auditRecord {
		t.Fatalf("kept %d bytes after 20 responses, want at least %d", n, 20*auditRecord)
	}
	post(t, fx, "/faults/f5/stop")
	get(t, proxy, "/k/a", "web")
	if a.stats.audited() != n || ad.active("f5") {
		t.Fatalf("after stop: %d bytes kept (want %d, no more growth and none freed)", a.stats.audited(), n)
	}
}
