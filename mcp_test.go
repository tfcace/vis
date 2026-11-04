package vis_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfcace/vis"
)

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// connect starts the instance's MCP endpoint and connects the SDK's own client to it.
func connect(t *testing.T, in *vis.Instance, token string, opts ...vis.MCPOption) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(in.MCP(opts...))
	t.Cleanup(srv.Close)
	return connectURL(t, srv.URL, token)
}

func connectURL(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	srv := struct{ URL string }{url}
	hc := &http.Client{Transport: http.DefaultTransport}
	if token != "" {
		hc.Transport = bearer{token, http.DefaultTransport}
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool and decodes its structured result; it fails the test on a protocol error.
func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		return map[string]any{"error": res.Content[0].(*mcp.TextContent).Text}, true
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out, false
}

var tokens = vis.Tokens(map[string]vis.Grant{
	"oncall-secret": {Writer: claude, Act: true},
	"viewer-secret": {Writer: vis.Writer{Kind: vis.Agent, ID: "viewer"}},
	"sam-secret":    {Writer: sam, Act: true},
})

func TestMCPListsTheTools(t *testing.T) {
	cs := connect(t, vis.NewInstance(), "oncall-secret", tokens)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	for _, want := range []string{"describe", "knobs", "metrics", "history", "rules", "set_knob", "set_rule", "revert", "freeze", "unfreeze", "impact", "profile"} {
		if !slices.Contains(names, want) {
			t.Fatalf("tools = %v, missing %s", names, want)
		}
	}
}

func TestMCPKnobsDescribeAndExplainThemselves(t *testing.T) {
	cs := connect(t, vis.NewInstance(), "viewer-secret", tokens)
	out, isErr := call(t, cs, "knobs", map[string]any{})
	if isErr {
		t.Fatal(out)
	}
	var found bool
	for _, k := range out["knobs"].([]any) {
		k := k.(map[string]any)
		if k["name"] == "test_timeout" {
			found = true
			if k["value"] != "2s" || k["type"] != "duration" || k["min"] != "100ms" {
				t.Fatalf("test_timeout = %v", k)
			}
		}
	}
	if !found {
		t.Fatal("test_timeout missing")
	}
}

// The writer comes from the token, never from the request.
func TestMCPChangesActAsTheTokensWriter(t *testing.T) {
	in := vis.NewInstance()
	cs := connect(t, in, "oncall-secret", tokens)
	out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_cache_size": 2000}, "for": "30m", "reason": "hit rate 71%"})
	if isErr {
		t.Fatal(out)
	}
	if out["lease"] == float64(0) || out["ends_at"] == nil {
		t.Fatalf("result = %v, want a lease and when it ends", out)
	}
	st, _ := in.Stack("test_cache_size")
	if st[0].By != claude || st[0].Value != 2000 || st[0].Layer != vis.AgentLayer {
		t.Fatalf("stack top = %+v, want agent:claude's 2000", st[0])
	}
}

func TestMCPValuesTakeTheKnobsOwnTerms(t *testing.T) {
	in := vis.NewInstance()
	cs := connect(t, in, "sam-secret", tokens) // test_timeout doesn't pull from agents
	if out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_timeout": "500ms"}, "for": "10m", "reason": "r"}); isErr {
		t.Fatal(out)
	}
	if got := timeout.Get(in.Snapshot()); got != 500*time.Millisecond {
		t.Fatalf("timeout = %v", got)
	}
	out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_timeout": 500}, "for": "10m", "reason": "r"})
	if !isErr || !strings.Contains(out["error"].(string), "unit") {
		t.Fatalf("a bare number for a duration: %v, want an error asking for a unit", out)
	}
}

func TestMCPReadTokensCannotAct(t *testing.T) {
	cs := connect(t, vis.NewInstance(), "viewer-secret", tokens)
	if _, isErr := call(t, cs, "knobs", map[string]any{}); isErr {
		t.Fatal("a read token can't read")
	}
	out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_cache_size": 2000}, "for": "30m", "reason": "r"})
	if !isErr || !strings.Contains(out["error"].(string), "read") {
		t.Fatalf("set_knob with a read token: %v", out)
	}
}

func TestMCPWithoutTokensServesReadOnly(t *testing.T) {
	cs := connect(t, vis.NewInstance(), "")
	if _, isErr := call(t, cs, "knobs", map[string]any{}); isErr {
		t.Fatal("can't read without tokens")
	}
	if _, isErr := call(t, cs, "freeze", map[string]any{"reason": "r"}); !isErr {
		t.Fatal("acted with no tokens configured")
	}
}

// An agent refused has to tell a person how to let it act, so the refusal
// carries the whole setup: the grant and the header.
func TestMCPRefusalSaysHowToLetAnAgentAct(t *testing.T) {
	for name, cs := range map[string]*mcp.ClientSession{
		"no tokens":  connect(t, vis.NewInstance(), ""),
		"read token": connect(t, vis.NewInstance(), "viewer-secret", tokens),
	} {
		out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_cache_size": 2000}, "for": "30m", "reason": "r"})
		if !isErr {
			t.Fatalf("%s: set_knob acted", name)
		}
		for _, want := range []string{"VIS_TOKENS=", ":act"} {
			if !strings.Contains(out["error"].(string), want) {
				t.Errorf("%s: refusal %q doesn't say %q", name, out["error"], want)
			}
		}
	}
	out, _ := call(t, connect(t, vis.NewInstance(), ""), "freeze", map[string]any{"reason": "r"})
	if !strings.Contains(out["error"].(string), "Authorization: Bearer") {
		t.Errorf("refusal %q doesn't name the header the agent's client sends", out["error"])
	}
}

func TestMCPRejectsUnknownTokens(t *testing.T) {
	in := vis.NewInstance()
	srv := httptest.NewServer(in.MCP(tokens))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer guess")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	req.Header.Del("Authorization") // no token at all: the answer says what to send
	req.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "Authorization: Bearer") {
		t.Fatalf("without a token: %d %q, want 401 saying what to send", res.StatusCode, body)
	}
}

func TestMCPAgentsCannotUnfreeze(t *testing.T) {
	in := vis.NewInstance()
	agent, person := connect(t, in, "oncall-secret", tokens), connect(t, in, "sam-secret", tokens)
	if out, isErr := call(t, agent, "freeze", map[string]any{"reason": "my change made it worse"}); isErr {
		t.Fatal(out)
	}
	if out, isErr := call(t, agent, "unfreeze", map[string]any{"reason": "r"}); !isErr || !strings.Contains(out["error"].(string), "person") {
		t.Fatalf("agent unfreeze: %v", out)
	}
	if out, isErr := call(t, person, "unfreeze", map[string]any{"reason": "looked at it"}); isErr {
		t.Fatal(out)
	}
}

func TestMCPHistoryAndMetricsAnswer(t *testing.T) {
	in := vis.NewInstance()
	cs := connect(t, in, "oncall-secret", tokens)
	call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_cache_size": 2000}, "for": "30m", "reason": "hit rate"})
	h, _ := call(t, cs, "history", map[string]any{"limit": 5})
	if s := h["stories"].([]any)[0].(map[string]any); s["title"] != "You set test_cache_size to 2000 MiB." {
		t.Fatalf("story as the agent reads it: %v", s["title"])
	}
	ev := h["events"].([]any)
	if last := ev[len(ev)-1].(map[string]any); last["kind"] != "change" {
		t.Fatalf("history = %v", h)
	}
	m, isErr := call(t, cs, "metrics", map[string]any{"window": "1m"})
	if isErr || m["runtime"] == nil {
		t.Fatalf("metrics = %v", m)
	}
}

func TestParseTokens(t *testing.T) {
	g, err := vis.ParseTokens("s3cret=agent:oncall:act, view=agent:viewer ,p=human:sam:act")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]vis.Grant{
		"s3cret": {Writer: vis.Writer{Kind: vis.Agent, ID: "oncall"}, Act: true},
		"view":   {Writer: vis.Writer{Kind: vis.Agent, ID: "viewer"}},
		"p":      {Writer: sam, Act: true},
	}
	if len(g) != len(want) {
		t.Fatalf("grants = %v", g)
	}
	for k, w := range want {
		if g[k] != w {
			t.Fatalf("grant %q = %+v, want %+v", k, g[k], w)
		}
	}
	for _, bad := range []string{"x", "x=robot:a", "x=agent:", "x=agent:a:write", "=agent:a"} {
		if _, err := vis.ParseTokens(bad); err == nil {
			t.Errorf("ParseTokens(%q) succeeded", bad)
		}
	}
}

func TestMCPSetRule(t *testing.T) {
	in := vis.NewInstance()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go in.RunRule(ctx, vis.RuleSpec{Name: "mcp_rule", Knob: batchSize, Watch: vis.Measure{Name: "zero", Read: func(*vis.Instance) float64 { return 0 }},
		Over: vis.Fixed(time.Millisecond), Step: -1, Lease: time.Minute})
	for len(in.Rules()) == 0 {
		time.Sleep(time.Millisecond)
	}
	cs := connect(t, in, "oncall-secret", tokens)
	out, isErr := call(t, cs, "set_rule", map[string]any{"rule": "mcp_rule", "enabled": false, "reason": "acting against the incident"})
	if isErr {
		t.Fatal(out)
	}
	if _, isErr := call(t, cs, "set_rule", map[string]any{"rule": "nope", "enabled": false, "reason": "r"}); !isErr {
		t.Fatal("disabled an unknown rule")
	}
}

// A change's reply says when its effect will be measured; impact waits for it, unless told not to.
func TestMCPImpactAnswersWhatAChangeDid(t *testing.T) {
	in := vis.NewInstance()
	cs := connect(t, in, "oncall-secret", tokens)
	out, _ := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_cache_size": 2100}, "for": "30m", "reason": "r"})
	if out["measured_at"] == nil {
		t.Fatalf("set_knob reply = %v, want when its effect will be measured", out)
	}
	im, isErr := call(t, cs, "impact", map[string]any{"change": out["change"], "no_wait": true})
	if isErr || im["done"] != false || !strings.Contains(im["summary"].(string), "Measuring") {
		t.Fatalf("impact right away = %v", im)
	}
	if _, isErr := call(t, cs, "impact", map[string]any{"change": 99999}); !isErr {
		t.Fatal("impact of an unknown change succeeded")
	}
}

// impact waits until the change is measured, so an agent never has to sleep.
func TestMCPImpactWaitsUntilMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		defer slowService(t, in)()
		time.Sleep(30 * time.Second)
		rec, _ := in.Apply(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		ct, st := mcp.NewInMemoryTransports()
		ss, err := vis.MCPServer(in).Connect(context.Background(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ss.Close()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		start := time.Now()
		im, isErr := call(t, cs, "impact", map[string]any{"change": rec.Change})
		if isErr || im["done"] != true || !strings.Contains(im["summary"].(string), "→ 10 ms") {
			t.Fatalf("impact = %v", im)
		}
		if waited := time.Since(start); waited < 30*time.Second {
			t.Fatalf("answered after %v, before the change was measured", waited)
		}
	})
}

// metrics splits a route by its probes, so an agent sees hits apart from misses.
func TestMCPMetricsShowProbes(t *testing.T) {
	in := vis.NewInstance()
	h := probedService(in)
	for i := range 20 {
		serve(h, "GET", "/k/"+strconv.Itoa(i))
	}
	m, isErr := call(t, connect(t, in, "viewer-secret", tokens), "metrics", map[string]any{})
	if isErr {
		t.Fatal(m)
	}
	r := m["routes"].([]any)[0].(map[string]any)
	if r["route"] != "GET /k/{key}" || len(r["probes"].([]any)) != 2 || len(r["paths"].([]any)) != 2 {
		t.Fatalf("route = %v, want its two probes and two paths", r)
	}
	if p := r["probes"].([]any)[1].(map[string]any); p["probe"] != "origin" || p["share"] != .5 {
		t.Fatalf("origin = %v", p)
	}
}

// profile reads a pprof profile for the agent; a read token may take one.
func TestMCPProfile(t *testing.T) {
	c := make(chan struct{})
	defer close(c)
	for range 200 {
		go parkForever(c)
	}
	in := vis.NewInstance()
	cs := connect(t, in, "viewer-secret", tokens)
	out, isErr := call(t, cs, "profile", map[string]any{"kind": "goroutine"})
	if isErr || !strings.Contains(out["summary"].(string), "parkForever") {
		t.Fatalf("profile = %v", out)
	}
	// Taking a profile is in the history: who took which, and what it found.
	// It has a cost (block and mutex sampling change the runtime while they run),
	// and an agent's reasoning is easier to follow with its evidence beside it.
	var got *vis.Event
	for _, e := range in.History(10) {
		if e.Kind == "profile" {
			got = &e
		}
	}
	if got == nil || got.Fields["actor"] != "agent:viewer" || got.Fields["kind"] != "goroutine" || !strings.Contains(got.Fields["summary"].(string), "parkForever") {
		t.Fatalf("history = %+v, want a profile event by agent:viewer", in.History(10))
	}
	open := vis.NewInstance() // no tokens: anyone may read, and is "agent" in the history
	if _, isErr := call(t, connect(t, open, ""), "profile", map[string]any{"kind": "goroutine"}); isErr {
		t.Fatal("profile without tokens failed")
	}
	if h := open.History(10); len(h) == 0 || h[len(h)-1].Kind != "profile" || h[len(h)-1].Fields["actor"] != "agent" {
		t.Fatalf("history without tokens = %+v", h)
	}
	if out, isErr := call(t, cs, "profile", map[string]any{"kind": "nope"}); !isErr || !strings.Contains(out["error"].(string), "goroutine") {
		t.Fatalf("unknown kind = %v", out)
	}
}

// When the measurement is late (here a published signal takes a minute to read),
// impact stops waiting and says so, rather than hang or read as a fresh change.
func TestMCPImpactSaysWhenItIsLate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		var slow atomic.Bool
		in.Signal("slow_signal", "", "takes a minute once asked to", func() float64 {
			if slow.Load() {
				time.Sleep(time.Minute)
			}
			return 1
		})
		rec, _ := in.Apply(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_slowness", Value: 10}}})
		synctest.Wait() // the before window is read
		slow.Store(true)
		cs := memoryClient(t, in)
		start := time.Now()
		im, isErr := call(t, cs, "impact", map[string]any{"change": rec.Change})
		if isErr || im["done"] != false || !strings.Contains(im["summary"].(string), "late") {
			t.Fatalf("impact = %v, want a pending report that says it is late", im)
		}
		if waited := time.Since(start); waited < 44*time.Second || waited > 46*time.Second {
			t.Fatalf("waited %v, want measured_at plus 15 s", waited)
		}
		time.Sleep(time.Minute) // let the slow read finish inside the bubble
	})
}

// memoryClient connects to the instance's MCP server in memory, so a test can run in a synctest bubble.
func memoryClient(t *testing.T, in *vis.Instance) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := vis.MCPServer(in).Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// Tool calls that take a while (impact waits, profiles sample) outlive a
// server's WriteTimeout, which would otherwise cut them off mid-answer.
func TestMCPLongCallsOutliveWriteTimeout(t *testing.T) {
	srv := httptest.NewUnstartedServer(vis.NewInstance().MCP(tokens))
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	out, isErr := call(t, connectURL(t, srv.URL, "viewer-secret"), "profile", map[string]any{"kind": "cpu", "seconds": 2})
	if isErr || out["kind"] != "cpu" {
		t.Fatalf("a 2 s profile behind a 1 s WriteTimeout = %v", out)
	}
}
