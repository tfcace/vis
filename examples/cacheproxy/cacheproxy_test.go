package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

var oncall = vis.Writer{Kind: vis.Agent, ID: "oncall"}

// startApp runs the proxy route and vis's control UI (as sam), the way main does.
func startApp(t *testing.T) (a *app, proxy, ui *httptest.Server) {
	t.Helper()
	a = newApp(vis.NewInstance(vis.WithLogger(slog.New(slog.DiscardHandler)), vis.Service("cacheproxy"), vis.SLO(keyRoute, 50*time.Millisecond)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.batch.run(ctx)
	proxy, ui = httptest.NewServer(a.routes()), httptest.NewServer(a.in.UI(vis.UIUser("sam")))
	t.Cleanup(proxy.Close)
	t.Cleanup(ui.Close)
	return a, proxy, ui
}

func set(t *testing.T, a *app, knob string, v int64) {
	t.Helper()
	if _, err := a.in.Apply(vis.Tx{By: oncall, For: 30 * time.Minute, Reason: "test", Set: []vis.Write{{Knob: knob, Value: v}}}); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, srv *httptest.Server, path, tenant string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant", tenant)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

func TestAnAgentChangeResizesTheCache(t *testing.T) {
	a, proxy, _ := startApp(t)
	set(t, a, "cache_size", 2000)
	if _, capacity := a.cache.size(); capacity != entriesFor(2000) {
		t.Fatalf("cache capacity = %d, want %d after OnChange", capacity, entriesFor(2000))
	}
	if code, _ := get(t, proxy, "/k/hello", "web"); code != 200 {
		t.Fatalf("proxy request: %d", code)
	}
}

func TestTheCompressionLevelIsReadPerRequest(t *testing.T) {
	a, proxy, _ := startApp(t)
	_, best := get(t, proxy, "/k/a", "web")
	set(t, a, "compress_level", 1)
	if _, fast := get(t, proxy, "/k/a", "web"); len(fast) <= len(best) {
		t.Fatalf("level 1 body = %d bytes, level 9 = %d; want level 1 larger", len(fast), len(best))
	}
}

func TestTheScanLimitTurnsAwayOnlyTheScanTenant(t *testing.T) {
	a, proxy, _ := startApp(t)
	set(t, a, "scan_limit", 0)
	if code, _ := get(t, proxy, "/k/x", "scan"); code != http.StatusTooManyRequests {
		t.Fatalf("scan: %d, want 429", code)
	}
	if code, _ := get(t, proxy, "/k/x", "web"); code != http.StatusOK {
		t.Fatalf("web: %d, want 200", code)
	}
}

// What a person or a script sees: the proxy's route, its SLO, what each knob costs, and the app's own signals.
func TestTheControlUIShowsTheProxy(t *testing.T) {
	_, proxy, ui := startApp(t)
	for range 5 {
		get(t, proxy, "/k/a", "web")
	}
	_, body := get(t, ui, "/api/state", "")
	var st struct {
		Service string `json:"service"`
		Routes  []struct {
			Route string  `json:"route"`
			SLO   float64 `json:"slo_ms"`
		} `json:"routes"`
		Knobs []struct {
			Name  string `json:"name"`
			About string `json:"about"`
		} `json:"knobs"`
		Signals []struct {
			Name string `json:"name"`
		} `json:"signals"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Service != "cacheproxy" || len(st.Routes) != 1 || st.Routes[0].Route != keyRoute || st.Routes[0].SLO != 50 {
		t.Fatalf("service %q, routes %+v: want only the proxy route, with its SLO", st.Service, st.Routes)
	}
	about := map[string]string{}
	for _, k := range st.Knobs {
		about[k.Name] = k.About
	}
	if about["compress_level"] == "" || about["scan_limit"] == "" {
		t.Fatalf("knobs without a description: %v", about)
	}
	signals := map[string]bool{}
	for _, s := range st.Signals {
		signals[s.Name] = true
	}
	for _, want := range []string{"hit_rate", "batch_wait_p99", "origin_keys_per_s", "tenant_rps.web"} {
		if !signals[want] {
			t.Fatalf("signals = %v, missing %s", signals, want)
		}
	}
}

// With real values, each entry holds what its share of cache_size says: a
// MiB of cache_size is 8 entries of 128 KiB, so the knob's memory is real.
func TestRealValuesHoldTheirShareOfCacheSize(t *testing.T) {
	c := newLRU(4)
	c.real = true
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		c.add(k)
	}
	if c.heldBytes() != 4*valueBytes {
		t.Fatalf("held %d bytes, want 4 entries of %d", c.heldBytes(), valueBytes)
	}
	if entriesFor(1)*valueBytes != 1<<20 {
		t.Fatalf("a MiB of cache_size is %d entries of %d bytes, not a MiB", entriesFor(1), valueBytes)
	}
	plain := newLRU(4)
	plain.add("a")
	if plain.heldBytes() != 0 {
		t.Fatal("without real values the cache holds values")
	}
}
