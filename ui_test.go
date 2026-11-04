package vis_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

func uiServer(t *testing.T, in *vis.Instance) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(in.UI(vis.UIUser("sam")))
	t.Cleanup(srv.Close)
	return srv
}

func uiCall(t *testing.T, srv *httptest.Server, method, path, body string, hdr ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	json.NewDecoder(res.Body).Decode(&m)
	return res.StatusCode, m
}

func TestUIServesThePageAndState(t *testing.T) {
	srv := uiServer(t, vis.NewInstance(vis.SLO("GET /k/{key}", 50*time.Millisecond)))
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	code, st := uiCall(t, srv, "GET", "/api/state", "")
	if code != 200 || st["user"] != "human:sam" || st["knobs"] == nil || st["freeze"] == nil {
		t.Fatalf("state: %d %v", code, st)
	}
	if slo := st["slo"].(map[string]any); slo["route"] != "GET /k/{key}" || slo["p99_ms"] != 50.0 {
		t.Fatalf("slo = %v", slo)
	}
}

// The page reads every list as an array: an empty one is [], never null,
// or a service with nothing declared yet (no knobs, no history) draws nothing.
func TestUIStateListsAreArraysWhenEmpty(t *testing.T) {
	srv := uiServer(t, vis.NewInstance())
	_, st := uiCall(t, srv, "GET", "/api/state", "")
	for _, k := range []string{"slos", "routes", "signals", "knobs", "rules", "history", "stories", "pressures", "warnings"} {
		if _, ok := st[k].([]any); !ok {
			t.Errorf("%s = %#v, want an array", k, st[k])
		}
	}
}

// The state says when the instance started: the page judges no SLO in the
// first minute after, when caches, connections and models are still loading.
func TestUIStateSaysWhenTheInstanceStarted(t *testing.T) {
	before := time.Now().Add(-time.Second)
	_, st := uiCall(t, uiServer(t, vis.NewInstance()), "GET", "/api/state", "")
	started, err := time.Parse(time.RFC3339, fmt.Sprint(st["started"]))
	if err != nil || started.Before(before.Truncate(time.Second)) || started.After(time.Now()) {
		t.Fatalf("started = %v (%v), want about now", st["started"], err)
	}
}

// The plan box asks before acting; changes from the UI are the configured person's pins.
func TestUIPreviewsThenPins(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
	srv := uiServer(t, in)
	code, p := uiCall(t, srv, "POST", "/api/preview", `{"set":{"test_batch_size":89},"for":"30m"}`)
	if code != 200 {
		t.Fatalf("preview: %d %v", code, p)
	}
	if e := p["effects"].([]any)[0].(map[string]any); e["value"] != 89.0 || e["layer"] != "pin" {
		t.Fatalf("preview effect = %v", e)
	}
	if batchSize.Get(in.Snapshot()) != 48 {
		t.Fatal("preview changed the knob")
	}
	code, rec := uiCall(t, srv, "POST", "/api/apply", `{"set":{"test_batch_size":89},"for":"30m","reason":"from the dial"}`)
	if code != 200 || rec["lease"] == 0.0 {
		t.Fatalf("apply: %d %v", code, rec)
	}
	if st, _ := in.Stack("test_batch_size"); st[0].By != sam || st[0].Layer != vis.PinLayer {
		t.Fatalf("stack top = %+v, want sam's pin", st[0])
	}
	if code, _ := uiCall(t, srv, "POST", "/api/clear", `{"knob":"test_batch_size","reason":"done"}`); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if batchSize.Get(in.Snapshot()) != 48 {
		t.Fatal("clearing the pin didn't fall back to the rule's 48")
	}
}

// On call, a person on the page could set a cache that fits, but not keep it:
// with a leak in the code, the next kill dropped it, and the service went
// back to the default that crash-loops. Agents keep values through set_knob.
func TestUIKeepsAValueAcrossARestart(t *testing.T) {
	in := vis.NewInstance()
	srv := uiServer(t, in)
	code, rec := uiCall(t, srv, "POST", "/api/apply", `{"set":{"test_batch_size":89},"for":"30m","keep":true,"reason":"fits"}`)
	if code != 200 {
		t.Fatalf("apply: %d %v", code, rec)
	}
	if st, _ := in.Stack("test_batch_size"); !st[0].Kept {
		t.Fatalf("stack top = %+v, want kept", st[0])
	}
	if code, rec := uiCall(t, srv, "POST", "/api/apply", `{"set":{"test_batch_size":90},"keep":true,"reason":"forever"}`); code == 200 {
		t.Fatalf("kept with no end: %v", rec)
	}
}

func TestUIFreezeAndUnfreeze(t *testing.T) {
	in := vis.NewInstance()
	srv := uiServer(t, in)
	if code, f := uiCall(t, srv, "POST", "/api/freeze", `{"reason":"investigating"}`); code != 200 || f["frozen"] != true {
		t.Fatalf("freeze: %d %v", code, f)
	}
	if code, f := uiCall(t, srv, "POST", "/api/unfreeze", `{"reason":"done"}`); code != 200 || f["frozen"] != false {
		t.Fatalf("unfreeze: %d %v", code, f)
	}
}

// Any web page the user visits could POST to localhost; only the UI's own page may.
func TestUIRefusesOtherOrigins(t *testing.T) {
	in := vis.NewInstance()
	srv := uiServer(t, in)
	code, _ := uiCall(t, srv, "POST", "/api/freeze", `{"reason":"x"}`, "Origin", "https://evil.example")
	if code != http.StatusForbidden || in.FreezeState().Frozen {
		t.Fatalf("cross-origin freeze: %d, frozen=%v", code, in.FreezeState().Frozen)
	}
	if code, _ := uiCall(t, srv, "POST", "/api/freeze", `{"reason":"x"}`, "Origin", srv.URL); code != 200 {
		t.Fatalf("same-origin freeze: %d", code)
	}
}

// DNS rebinding: a hostname that resolves to 127.0.0.1 must not reach the API.
func TestUIRefusesOtherHosts(t *testing.T) {
	srv := uiServer(t, vis.NewInstance())
	req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
	req.Host = "attacker.example:80"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("other host: %d, want 403", res.StatusCode)
	}
}

func TestUISwitchesRules(t *testing.T) {
	in := vis.NewInstance()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go in.RunRule(ctx, vis.RuleSpec{Name: "ui_rule", Knob: batchSize, Watch: vis.Measure{Name: "zero", Read: func(*vis.Instance) float64 { return 0 }},
		Over: vis.Fixed(time.Millisecond), Step: -1, Lease: time.Minute})
	for len(in.Rules()) == 0 {
		time.Sleep(time.Millisecond)
	}
	srv := uiServer(t, in)
	if code, out := uiCall(t, srv, "POST", "/api/rule", `{"rule":"ui_rule","enabled":false,"reason":"acting against the incident"}`); code != 200 {
		t.Fatalf("rule: %d %v", code, out)
	}
	for _, r := range in.Rules() {
		if r.Name == "ui_rule" && r.State != "disabled" {
			for r.State != "disabled" { // the rule notices on its next check
				time.Sleep(10 * time.Millisecond)
				r = in.Rules()[0]
			}
		}
	}
}

// One contract: every key in the state the page and scripts read is snake_case.
func TestUIStateIsSnakeCaseThroughout(t *testing.T) {
	in := vis.NewInstance(vis.Service("demo"), vis.SLO("GET /k/{key}", 50*time.Millisecond))
	in.Signal("hit_rate", "", "Share of lookups served from the cache.", func() float64 { return .9 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go in.RunRule(ctx, vis.RuleSpec{Name: "r", Knob: batchSize, Watch: vis.SchedP99, Over: timeout, Step: -1, Lease: time.Minute})
	for len(in.Rules()) == 0 {
		time.Sleep(time.Millisecond)
	}
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: claude, Reason: "r"})
	srv := uiServer(t, in)
	_, st := uiCall(t, srv, "GET", "/api/state", "")
	if st["api"] != 1.0 {
		t.Fatalf("api version = %v", st["api"])
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if k != strings.ToLower(k) || strings.ContainsAny(k, " -") {
					t.Errorf("%s.%s is not snake_case", path, k)
				}
				walk(path+"."+k, e)
			}
		case []any:
			for _, e := range x {
				walk(path+"[]", e)
			}
		}
	}
	walk("state", st)
}
