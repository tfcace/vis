package vis_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// peer starts an instance of service on the in-memory network at name:7946,
// seeded from seed, and returns it with a function that stops it.
func peer(t *testing.T, net *pipenet.Network, service, name, key, seed string, extra ...any) (*vis.Instance, func()) {
	t.Helper()
	ln, err := net.Listen(name + ":7946")
	if err != nil {
		t.Fatal(err)
	}
	iopts := []vis.InstanceOption{vis.Service(service), vis.InstanceName(name)}
	var sopts []vis.ServeOption
	for _, o := range extra {
		switch o := o.(type) {
		case vis.InstanceOption:
			iopts = append(iopts, o)
		case vis.ServeOption:
			sopts = append(sopts, o)
		}
	}
	in := vis.NewInstance(iopts...)
	ctx, cancel := context.WithCancel(context.Background())
	opts := []vis.ServeOption{vis.MCPAt("", tokens), vis.UIAt(""), vis.PeerListener(ln), vis.PeerDialer(net.Dial),
		vis.PeerAdvertise(name + ":7946"), vis.ClusterKey(key)}
	if seed != "" {
		opts = append(opts, vis.Seeds(seed))
	}
	opts = append(opts, sopts...)
	if _, err := in.Serve(ctx, opts...); err != nil {
		cancel()
		t.Fatal(err)
	}
	return in, cancel
}

// members lists who in sees, as "name:state", itself first.
func members(in *vis.Instance) []string {
	var out []string
	for _, m := range in.Members() {
		out = append(out, m.Name+":"+m.State)
	}
	return out
}

// Instances of one service seeded from one of them find each other.
func TestARingFormsFromOneSeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		for _, in := range []*vis.Instance{a, b, c} {
			got := members(in)
			if len(got) != 3 || !slices.Equal(got[1:], slices.DeleteFunc([]string{"a:alive", "b:alive", "c:alive"}, func(s string) bool { return s == got[0] })) {
				t.Errorf("%s sees %v, want all three alive", got[0], got)
			}
			if m := in.Members()[0]; !m.Self || m.Service != "cacheproxy" || m.Addr == "" || m.ID == "" {
				t.Errorf("first member = %+v, want itself, with its service, address and ID", m)
			}
		}
	})
}

// A member that stops cleanly says so, and is gone at once: a scale-down or a
// rolling deploy isn't an incident.
func TestAMemberThatLeavesIsGoneAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		_, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		stopB()
		time.Sleep(3 * time.Second)
		for _, in := range []*vis.Instance{a, c} {
			if got := members(in); slices.Contains(got, "b:alive") || slices.Contains(got, "b:suspect") || len(got) != 2 {
				t.Fatalf("after b left: %v, want b gone", got)
			}
		}
	})
}

// A member that goes silent without leaving may still be taking traffic: it is
// suspect after 5 s, dead after 15, and stays listed, dead, for 10 minutes.
func TestAMemberThatGoesSilentStaysListedDead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b) // as a crash, a kill or a hang would
		stopB()
		for _, step := range []struct {
			after time.Duration
			want  []string
		}{
			{7 * time.Second, []string{"a:alive", "b:suspect"}},
			{10 * time.Second, []string{"a:alive", "b:dead"}},
			{5 * time.Minute, []string{"a:alive", "b:dead"}},
			{6 * time.Minute, []string{"a:alive"}},
		} {
			time.Sleep(step.after)
			if got := members(a); !slices.Equal(got, step.want) {
				t.Fatalf("after another %v: %v, want %v", step.after, got, step.want)
			}
		}
	})
}

// Only instances of the same service holding the same key join: a leaked key
// stays inside one service, and a stranger is never a member.
func TestARingAdmitsOnlyItsServiceWithItsKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		_, stopB := peer(t, n, "cacheproxy", "b", "wrong", "a:7946")
		_, stopC := peer(t, n, "imageproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(5 * time.Second)
		if got := members(a); !slices.Equal(got, []string{"a:alive"}) {
			t.Fatalf("a sees %v, want only itself", got)
		}
		// A key that differs is a misconfiguration someone has to find: say so.
		if w := strings.Join(a.Warnings(), "\n"); !strings.Contains(w, "VIS_CLUSTER_KEY differs") {
			t.Fatalf("warnings = %q, want the refused peer named", w)
		}
	})
}

// Seeds without a key would join nothing: Serve says what's missing.
func TestSeedsWithoutAKeyFailServe(t *testing.T) {
	in := vis.NewInstance()
	_, err := in.Serve(context.Background(), vis.MCPAt(""), vis.UIAt(""), vis.Seeds("a:7946"))
	if err == nil || !strings.Contains(err.Error(), "VIS_CLUSTER_KEY") {
		t.Fatalf("Serve = %v, want it to ask for VIS_CLUSTER_KEY", err)
	}
}

// Without peers an instance is a ring of one: itself.
func TestAnInstanceWithoutPeersIsItsOwnFleet(t *testing.T) {
	in := vis.NewInstance(vis.InstanceName("solo"))
	got := in.Members()
	if len(got) != 1 || !got[0].Self || got[0].Name != "solo" || got[0].State != "alive" {
		t.Fatalf("members = %s, want only itself", fmt.Sprint(got))
	}
}

// connectOver connects an MCP client to in over the in-memory network, as an
// agent reaching one instance of the fleet, with token.
func connectOver(t *testing.T, n *pipenet.Network, in *vis.Instance, addr, token string) *mcp.ClientSession {
	t.Helper()
	ln, err := n.Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: in.MCP(tokens)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	hc := &http.Client{Transport: bearer{token, &http.Transport{DialContext: n.Dial}}}
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: "http://" + addr, HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); hc.CloseIdleConnections() })
	return cs
}

var fleetKnob = vis.NewInt("fleet_test_knob", vis.Range(1, 9), vis.Default(5), vis.Pull(vis.Agent))

// An agent connected to one instance reads and acts on another by naming it:
// the call runs there, under the agent's own token, which that member checks.
func TestAToolRunsOnTheMemberItNames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")

		out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "b", "set": map[string]any{"fleet_test_knob": 8}, "for": "10m", "reason": "only b"})
		if isErr {
			t.Fatalf("set_knob on b: %v", out["error"])
		}
		if fleetKnob.Get(b.Snapshot()) != 8 || fleetKnob.Get(a.Snapshot()) != 5 {
			t.Fatalf("b = %d, a = %d; want only b changed", fleetKnob.Get(b.Snapshot()), fleetKnob.Get(a.Snapshot()))
		}
		st, _ := b.Stack("fleet_test_knob")
		if st[0].By != claude {
			t.Fatalf("b's change is by %v, want the token's writer, %v", st[0].By, claude)
		}
		k, _ := call(t, cs, "knobs", map[string]any{"instance": "b", "knob": "fleet_test_knob"})
		if v := k["knobs"].([]any)[0].(map[string]any)["value"]; v != float64(8) {
			t.Fatalf("knobs on b reads %v, want 8", v)
		}

		viewer := connectOver(t, n, a, "a:7071", "viewer-secret")
		if out, isErr := call(t, viewer, "set_knob", map[string]any{"instance": "b", "set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"}); !isErr || !strings.Contains(fmt.Sprint(out["error"]), "can read but not act") {
			t.Fatalf("a read-only token acting on b: %v, want b to refuse it", out)
		}
		if out, isErr := call(t, cs, "knobs", map[string]any{"instance": "z"}); !isErr || !strings.Contains(fmt.Sprint(out["error"]), "a, b") {
			t.Fatalf("an unknown member: %v, want an error naming the members", out)
		}
	})
}

// The fleet tool is the comparative question: each member against the rest,
// percentiles merged from histograms, and who stands out, with how to look closer.
func TestFleetSaysWhichMemberStandsOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		serveFor := func(in *vis.Instance, d time.Duration) {
			for range 50 {
				_, end := in.StartRequest(context.Background())
				time.Sleep(d)
				end("GET /fleet/{k}", 200)
			}
		}
		go serveFor(a, 10*time.Millisecond)
		go serveFor(b, 200*time.Millisecond)
		go serveFor(c, 10*time.Millisecond)
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 9, By: sam}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(11 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "fleet", nil)
		if isErr {
			t.Fatalf("fleet: %v", out["error"])
		}
		if got := len(out["members"].([]any)); got != 3 || out["covers"] != "3 of 3 members" {
			t.Fatalf("members = %d, covers %v; want 3 of 3", got, out["covers"])
		}
		slos := out["slos"].([]any)[0].(map[string]any)
		if p := slos["fleet_p99_ms"].(float64); p < 150 || slos["met"] != false { // 1 in 3 requests took 200 ms: the merged p99 is b's, not an average
			t.Fatalf("fleet p99 = %v, met %v; want about 200 ms, missed", p, slos["met"])
		}
		stand := fmt.Sprint(out["stand_out"])
		if !strings.Contains(stand, "b") || strings.Contains(stand, "member:a") || strings.Contains(stand, "member:c") || !strings.Contains(stand, "instance") {
			t.Fatalf("stand_out = %s, want b alone, with how to drill in", stand)
		}
		if d := fmt.Sprint(out["differ"]); !strings.Contains(d, "fleet_test_knob") || !strings.Contains(d, "b:9") || !strings.Contains(d, "b:human:sam") {
			t.Fatalf("differ = %s, want fleet_test_knob at 9 on b, set by sam", d)
		}
		// Why b stands out names what is different about it, not only its symptoms.
		if !strings.Contains(stand, "its fleet_test_knob is 9 (set by human:sam), while the others have 5") {
			t.Fatalf("stand_out = %s, want b's difference named as a cause", stand)
		}
	})
}

// A freeze on any member holds the whole fleet, saying who froze it, why and
// where; only a person unfreezes, on any member, and that reaches all of them.
func TestAFreezeReachesTheFleet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		if err := a.Freeze(sam, "investigating the spike"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		for _, in := range []*vis.Instance{b, c} {
			f := in.FreezeState()
			if !f.Frozen || f.By != sam || f.Reason != "investigating the spike" || f.On != "a" {
				t.Fatalf("%s: freeze = %+v, want frozen by sam on a, with the reason", in.Members()[0].Name, f)
			}
		}
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 7, By: claude, For: time.Minute}); err == nil {
			t.Fatal("an agent changed a knob on a frozen member")
		}
		if err := b.Unfreeze(claude, "done"); err == nil {
			t.Fatal("an agent unfroze a member")
		}
		if err := c.Unfreeze(sam, "spike explained"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		for _, in := range []*vis.Instance{a, b, c} {
			if f := in.FreezeState(); f.Frozen || f.By != sam || f.On != "c" {
				t.Fatalf("%s: freeze = %+v, want unfrozen by sam on c", in.Members()[0].Name, f)
			}
		}
	})
}

// A member too starved to answer is the one that matters most: the fleet view
// still has it from what its gossip carries, and says it didn't answer.
func TestAMemberTooBusyToAnswerStillStandsOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		vis.StallSummaries(b, time.Minute)
		for in, d := range map[*vis.Instance]time.Duration{a: 10 * time.Millisecond, b: 200 * time.Millisecond, c: 10 * time.Millisecond} {
			go func() {
				for range 50 {
					_, end := in.StartRequest(context.Background())
					time.Sleep(d)
					end("GET /fleet/{k}", 200)
				}
			}()
		}
		time.Sleep(11 * time.Second)
		rep := a.Fleet(context.Background())
		if !strings.Contains(rep.Covers, "b") || !strings.Contains(rep.Covers, "gossip") {
			t.Fatalf("covers = %q, want it to say b came from gossip", rep.Covers)
		}
		var why []string
		for _, s := range rep.StandOut {
			if s.Member == "b" {
				why = append(why, s.Why)
			}
		}
		if w := strings.Join(why, "; "); !strings.Contains(w, "didn't answer") || !strings.Contains(w, "over its objective") {
			t.Fatalf("b stands out for %q, want that it didn't answer and is over its objective", w)
		}
		if s := rep.SLOs[0]; s.Met || s.P99 < 150*time.Millisecond {
			t.Fatalf("fleet objective = %+v, want missed at about 200 ms: b's requests merged from its gossiped histogram", s)
		}
	})
}

// An instance restarted under its name replaces its dead self: the old one
// isn't left standing out.
func TestARestartedMemberReplacesItsDeadSelf(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b)
		stopB()
		time.Sleep(20 * time.Second)
		_, stopB2 := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB2(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		if got := members(a); !slices.Equal(got, []string{"a:alive", "b:alive"}) {
			t.Fatalf("after b restarted: %v, want only the new b", got)
		}
	})
}

// Silence is a finding, never absence: a member not heard from stands out, and
// the fleet can't be judged within its objective while one is unaccounted for.
func TestASilentMemberStandsOutAndTheFleetCantBeVouchedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		defer func() { stopA(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b)
		stopB()
		time.Sleep(20 * time.Second)
		rep := a.Fleet(context.Background())
		var why string
		for _, s := range rep.StandOut {
			if s.Member == "b" {
				why = s.Why
			}
		}
		if !strings.Contains(why, "not heard from") || !strings.Contains(why, "traffic") {
			t.Fatalf("b stands out for %q, want that it went silent and may still take traffic", why)
		}
		if s := rep.SLOs[0]; s.Met || !strings.Contains(s.Partial, "can't vouch for b") {
			t.Fatalf("fleet objective = %+v, want not met: b is unaccounted for", s)
		}
		if len(rep.Members) != 2 {
			t.Fatalf("members = %d, want b listed too", len(rep.Members))
		}
	})
}

// How long the fleet view waits for members is the operator's: a member that
// answers within it is merged as itself, not from gossip.
func TestFleetWaitIsConfigurable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.FleetWait(10*time.Second))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.StallSummaries(b, 5*time.Second)
		if rep := a.Fleet(context.Background()); rep.Covers != "2 of 2 members" {
			t.Fatalf("covers = %q, want b's answer, slow but within 10 s", rep.Covers)
		}
	})
}

// A person on one member's page reads and acts on another through it: inside
// a ring, the member trusts the page's person, as the user decided. The
// change is that person's, on that member alone.
func TestThePageActsOnAnotherMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		ln, _ := n.Listen("a:7071")
		srv := &http.Server{Handler: a.UI(vis.UIUser("sam"))}
		go srv.Serve(ln)
		defer srv.Close()
		hc := &http.Client{Transport: &http.Transport{DialContext: n.Dial}}
		defer hc.CloseIdleConnections()
		do := func(method, path, body string) (int, string) {
			req, _ := http.NewRequest(method, "http://localhost:7071"+path, strings.NewReader(body))
			req.URL.Host = "a:7071" // dial a, but say localhost, as a browser on the machine would
			req.Host = "localhost:7071"
			res, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var b strings.Builder
			io.Copy(&b, res.Body)
			return res.StatusCode, b.String()
		}
		selfOf := func(body string) string { // the member a state says it is
			var st struct {
				Fleet struct {
					Members []struct {
						Name string `json:"name"`
						Self bool   `json:"self"`
					} `json:"members"`
				} `json:"fleet"`
			}
			json.Unmarshal([]byte(body), &st)
			for _, m := range st.Fleet.Members {
				if m.Self {
					return m.Name
				}
			}
			return ""
		}
		if code, body := do("GET", "/api/state?instance=b", ""); code != 200 || selfOf(body) != "b" {
			t.Fatalf("b's state through a: %d, from %q", code, selfOf(body))
		}
		code, body := do("POST", "/api/apply?instance=b", `{"set":{"fleet_test_knob":3},"reason":"from a's page"}`)
		if code != 200 {
			t.Fatalf("apply on b through a: %d %s", code, body)
		}
		st, _ := b.Stack("fleet_test_knob")
		if fleetKnob.Get(b.Snapshot()) != 3 || fleetKnob.Get(a.Snapshot()) != 5 || st[0].By != sam {
			t.Fatalf("b = %d by %v, a = %d; want only b, pinned by sam", fleetKnob.Get(b.Snapshot()), st[0].By, fleetKnob.Get(a.Snapshot()))
		}
		if code, body := do("GET", "/api/state?instance=z", ""); code != 404 || !strings.Contains(body, "a, b") {
			t.Fatalf("an unknown member: %d %s, want 404 naming the members", code, body)
		}
	})
}

// Gossip relays what members knew, stale entries too: a restarted member's
// former self, relayed by a third member, must not come back alive.
func TestAReplacedMemberStaysGoneWhenOthersRelayIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		time.Sleep(3 * time.Second)
		vis.SkipLeave(c)
		stopC()
		time.Sleep(20 * time.Second)
		c2, stopC2 := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); stopC2(); synctest.Wait() }()
		for range 5 {
			time.Sleep(4 * time.Second)
			for _, in := range []*vis.Instance{a, b, c2} {
				if got := members(in); len(got) != 3 {
					t.Fatalf("%s sees %v, want a, b and one c", got[0], got)
				}
			}
		}
	})
}

// A member heard of second-hand is as fresh as the one relaying it knew it to
// be: a stale entry arrives stale, not alive.
func TestAStaleMemberArrivesStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		time.Sleep(3 * time.Second)
		vis.SkipLeave(c)
		stopC()
		time.Sleep(20 * time.Second) // a has c dead
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		var state string
		for _, m := range b.Members() {
			if m.Name == "c" {
				state = m.State
			}
		}
		if state != "dead" {
			t.Fatalf("b, which joined after c died, sees c %q, want dead: as stale as a knew it", state)
		}
		_, _ = a, c
	})
}

// A member whose port refuses the connection isn't busy, it isn't there: it
// is unaccounted for at once, before its heartbeat says so.
func TestARefusedMemberIsUnaccountedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		defer func() { stopA(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b)
		stopB()
		time.Sleep(1500 * time.Millisecond) // still alive by its heartbeat
		rep := a.Fleet(context.Background())
		var why string
		for _, s := range rep.StandOut {
			if s.Member == "b" {
				why = s.Why
			}
		}
		if !strings.Contains(why, "refused") || strings.Contains(why, "too busy") || rep.SLOs[0].Met {
			t.Fatalf("b stands out for %q, fleet met %v; want refused, not busy, and the fleet not vouched for", why, rep.SLOs[0].Met)
		}
	})
}

// Stop is what main defers: it tells the fleet this member is leaving and
// returns once it has, so a process that exits right after still left cleanly.
func TestStopLeavesBeforeItReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, _ := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		b.Stop()
		if got := members(a); len(got) != 1 {
			t.Fatalf("right after b.Stop returned, a sees %v, want b gone", got)
		}
		b.Stop() // twice is harmless
	})
}

// An instance none of whose seeds answer is alone, and others can't find it:
// it says so, with what to do about it.
func TestAnInstanceNoSeedAnswersSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "gone:7946")
		defer func() { stopA(); synctest.Wait() }()
		time.Sleep(6 * time.Second)
		if w := strings.Join(a.Warnings(), "\n"); !strings.Contains(w, "No seed answered") || !strings.Contains(w, "gone:7946") {
			t.Fatalf("warnings = %q, want that no seed answered, naming it", w)
		}
	})
}

// A call forwarded to a member that accepts connections and never answers (a
// paused container) fails in bounded time, naming the member, instead of
// holding the agent until its own client gives up.
func TestAForwardedCallToAHungMemberIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.StallPeers(b, time.Hour)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		start := time.Now()
		out, isErr := call(t, cs, "knobs", map[string]any{"instance": "b"})
		if took := time.Since(start); !isErr || took > 15*time.Second || !strings.Contains(fmt.Sprint(out["error"]), "b") {
			t.Fatalf("a call to a hung b: %v after %v, want an error naming b within 15 s", out, took)
		}
		vis.StallPeers(b, 0)
	})
}

// In a fleet, a change names its member: an agent reaching the fleet through a
// load balancer lands on any of them, and a change that went wherever the call
// arrived, with an ID that is only that member's, is one nobody can find.
func TestInAFleetAChangeNamesItsMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		_, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"fleet_test_knob": 4}, "for": "5m", "reason": "somewhere"})
		if e := fmt.Sprint(out["error"]); !isErr || !strings.Contains(e, "instance") || !strings.Contains(e, "a, b") {
			t.Fatalf("set_knob without instance in a fleet: %v, want it refused, naming the members", out)
		}
		out, isErr = call(t, cs, "set_knob", map[string]any{"instance": "a", "set": map[string]any{"fleet_test_knob": 4}, "for": "5m", "reason": "here"})
		if isErr || out["instance"] != "a" {
			t.Fatalf("set_knob on a: %v, want the change with its member named", out)
		}
		out, isErr = call(t, cs, "set_knob", map[string]any{"instance": "b", "set": map[string]any{"fleet_test_knob": 6}, "for": "5m", "reason": "there"})
		if isErr || out["instance"] != "b" {
			t.Fatalf("set_knob on b through a: %v, want b named", out)
		}
	})
}

// The fleet shows what each member holds above its defaults, who set it and
// when it ends: an agent's fix on one member lapses with its lease, and
// whoever looks next should see that coming.
func TestTheFleetShowsWhatEachMemberHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: 10 * time.Minute, Reason: "fix"}); err != nil {
			t.Fatal(err)
		}
		rep := a.Fleet(context.Background())
		for _, m := range rep.Members {
			if m.Name != "b" {
				continue
			}
			if len(m.Held) != 1 || m.Held[0].Knob != "fleet_test_knob" || m.Held[0].Value != "8" || m.Held[0].By != claude.String() || m.Held[0].EndsIn < 9*time.Minute {
				t.Fatalf("b holds %+v, want fleet_test_knob 8 by %v for about 10 minutes", m.Held, claude)
			}
			return
		}
		t.Fatal("no b in the fleet")
	})
}

// A member restarted under its name lost what was set on it at runtime: the
// fleet says so, and a member restarting again and again stands out.
func TestARestartThatLostAChangeSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		time.Sleep(3 * time.Second)
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: 10 * time.Minute, Reason: "fix"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second) // its digest carries what it holds
		vis.SkipLeave(b)
		stopB()
		time.Sleep(time.Second) // its port closes
		_, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		time.Sleep(8 * time.Second)
		why := func() string {
			var w []string
			for _, s := range a.Fleet(context.Background()).StandOut {
				if s.Member == "b" {
					w = append(w, s.Why)
				}
			}
			return strings.Join(w, "; ")
		}
		if w := why(); !strings.Contains(w, "restarted") || !strings.Contains(w, "fleet_test_knob 8") {
			t.Fatalf("b stands out for %q, want its restart and the change it lost", w)
		}
		for range 2 { // and again, and again: a crash loop
			stop()
			time.Sleep(time.Second)
			_, stop = peer(t, n, "cacheproxy", "b", "k", "a:7946")
			time.Sleep(8 * time.Second)
		}
		defer func() { stopA(); stop(); synctest.Wait() }()
		if w := why(); !strings.Contains(w, "restarted 3 times") {
			t.Fatalf("b stands out for %q, want 3 restarts named", w)
		}
	})
}

// A person forgets a member that is gone for good (scaled down, its pod
// renamed), on every member, so it stops reading as unaccounted for; an
// agent may not, and nobody can forget a member that is alive.
func TestAPersonForgetsADeadMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		if err := a.Forget(sam, "b", "no"); err == nil {
			t.Fatal("forgot b, which is alive")
		}
		vis.SkipLeave(c)
		stopC()
		time.Sleep(20 * time.Second)
		if err := a.Forget(claude, "c", "scaled down"); err == nil {
			t.Fatal("an agent forgot a member")
		}
		if err := a.Forget(sam, "c", "scaled down"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		for _, in := range []*vis.Instance{a, b} {
			if got := members(in); len(got) != 2 {
				t.Fatalf("%s still lists c: %v", got[0], got)
			}
		}
	})
}

// A change on one member is measured against the rest of the fleet over the
// same windows: "b 200 → 10 ms while the rest held 10 → 10" says the change
// did it, where b alone could have been the load easing everywhere.
func TestAChangeIsComparedWithTheRestOfTheFleet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		var slow atomic.Bool
		slow.Store(true)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		for _, in := range []*vis.Instance{a, b, c} {
			load.Go(func() {
				for ctx.Err() == nil {
					_, end := in.StartRequest(ctx)
					d := 10 * time.Millisecond
					if in == b && slow.Load() {
						d = 200 * time.Millisecond
					}
					time.Sleep(d)
					end("GET /fleet/{k}", 200)
				}
			})
		}
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		rec, err := b.Apply(vis.Tx{By: claude, For: 10 * time.Minute, Reason: "fix", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 2}}})
		if err != nil {
			t.Fatal(err)
		}
		slow.Store(false)
		im, err := b.WaitImpact(ctx, rec.Change)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(im.Summary)
		if im.Rest == nil || len(im.Rest.Members) != 2 || !strings.Contains(im.Summary, "the rest of the fleet (a and c)") || !strings.Contains(im.Summary, "not an inference") {
			t.Fatalf("impact = %+v\nsummary %q; want b compared with a and c, labelled honestly", im.Rest, im.Summary)
		}
		if bf, af := im.Rest.Before["GET /fleet/{k} p99"], im.Rest.After["GET /fleet/{k} p99"]; bf > 15 || af > 15 {
			t.Fatalf("the rest = %v → %v, want about 10 → 10", bf, af)
		}
	})
}

// fleetLoad serves requests on each member whose latency follows its
// fleet_test_knob: lat(value). It stops with ctx.
func fleetLoad(ctx context.Context, wg *sync.WaitGroup, lat func(int) time.Duration, ins ...*vis.Instance) {
	for _, in := range ins {
		wg.Go(func() {
			for ctx.Err() == nil {
				_, end := in.StartRequest(ctx)
				time.Sleep(lat(fleetKnob.Get(in.Snapshot())))
				end("GET /fleet/{k}", 200)
			}
		})
	}
}

// A rollout changes one member first, compares it with the rest, and goes on
// to the rest only if that member got no worse than they did.
func TestARolloutGoesStageByStage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(v int) time.Duration { // the default, 5, is slow everywhere; 2 is fast
			if v <= 2 {
				return 10 * time.Millisecond
			}
			return 200 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "the fleet is slow"})
		if isErr {
			t.Fatalf("rollout: %v", out["error"])
		}
		stages := out["stages"].([]any)
		if len(stages) != 2 || len(stages[0].(map[string]any)["members"].([]any)) != 1 || out["outcome"] != "done" {
			t.Fatalf("rollout = %v, want one member, then the rest, done", out)
		}
		for _, in := range []*vis.Instance{a, b, c} {
			if fleetKnob.Get(in.Snapshot()) != 2 {
				t.Fatalf("%s holds %d, want 2 everywhere", in.Members()[0].Name, fleetKnob.Get(in.Snapshot()))
			}
		}
		if e := fmt.Sprint(stages[0].(map[string]any)["evidence"]); !strings.Contains(e, "the rest") {
			t.Fatalf("stage 1's evidence = %q, want it compared with the rest", e)
		}
	})
}

// A rollout that hurts its first member stops there and undoes it: the rest
// never see it.
func TestAHarmfulRolloutStopsAtTheFirstMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(v int) time.Duration { // 9 is slow; the default, 5, is fine
			if v >= 9 {
				return 300 * time.Millisecond
			}
			return 10 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"})
		if isErr {
			t.Fatalf("rollout: %v", out["error"])
		}
		if out["outcome"] != "stopped" || len(out["stages"].([]any)) != 1 || !strings.Contains(fmt.Sprint(out["why"]), "worse") {
			t.Fatalf("rollout = %v, want it stopped after the first member, saying it got worse", out)
		}
		for _, in := range []*vis.Instance{a, b, c} {
			if fleetKnob.Get(in.Snapshot()) != 5 {
				t.Fatalf("%s holds %d, want 5 everywhere: stage 1 undone, the rest untouched", in.Members()[0].Name, fleetKnob.Get(in.Snapshot()))
			}
		}
	})
}

// A rollout during a freeze changes nothing and says why; a member where a
// person's pin holds the knob is skipped as stage 1, since the change can't be
// measured where it doesn't take effect.
func TestARolloutRespectsFreezesAndPins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(int) time.Duration { return 10 * time.Millisecond }, a, b, c)
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")

		a.Freeze(sam, "hold")
		time.Sleep(3 * time.Second)
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 3}, "for": "10m", "reason": "while frozen"})
		if !isErr || !strings.Contains(fmt.Sprint(out["error"]), "frozen") || fleetKnob.Get(b.Snapshot()) == 3 {
			t.Fatalf("a rollout during a freeze: %v, want it refused, saying frozen, nothing changed", out)
		}
		a.Unfreeze(sam, "go")
		time.Sleep(3 * time.Second)

		order := map[string]*vis.Instance{"a": a, "b": b, "c": c}
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		if _, err := order[first].Set(vis.Change{Knob: "fleet_test_knob", Value: 7, By: sam, Reason: "pinned"}); err != nil {
			t.Fatal(err)
		}
		out, isErr = call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 3}, "for": "10m", "reason": "around the pin"})
		if isErr {
			t.Fatalf("rollout: %v", out["error"])
		}
		s1 := out["stages"].([]any)[0].(map[string]any)
		if fmt.Sprint(s1["masked"]) != fmt.Sprintf("[%s]", first) || fmt.Sprint(s1["members"]) == fmt.Sprintf("[%s]", first) || out["outcome"] != "done" {
			t.Fatalf("rollout = %v, want %s skipped as masked, stage 1 on another, done", out, first)
		}
	})
}

// A member starved of CPU answers slowly but does answer, and it is the one
// that needs the fix: a change forwarded to it waits longer than a read does.
func TestAChangeWaitsLongerForAStarvedMemberThanARead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.StallPeers(b, 6*time.Second) // each request it gets, the handshake and the call alike
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		if out, isErr := call(t, cs, "knobs", map[string]any{"instance": "b"}); !isErr {
			t.Fatalf("a read of a member 6 s slow: %v, want it to give up", out)
		}
		out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "b", "set": map[string]any{"fleet_test_knob": 3}, "for": "5m", "reason": "fix the starved one"})
		if isErr || fleetKnob.Get(b.Snapshot()) != 3 {
			t.Fatalf("a change to a member 6 s slow: %v, b holds %d; want it made", out, fleetKnob.Get(b.Snapshot()))
		}
		vis.StallPeers(b, 0)
	})
}

// A first stage the gate can't judge (here, too few requests) stops the
// rollout but isn't undone: undoing what it couldn't judge threw away working
// fixes. The member keeps it under its lease, and the result says how to revert.
func TestARolloutItCantJudgeStopsWithoutUndoing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(int) time.Duration { return 2 * time.Second }, a, b, c) // ten requests a window
		time.Sleep(65 * time.Second)                                                       // past the first minute, when a change can be judged
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "quiet service"})
		if isErr {
			t.Fatalf("rollout: %v", out["error"])
		}
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		held := map[string]*vis.Instance{"a": a, "b": b, "c": c}[first]
		if out["outcome"] != "stopped" || !strings.Contains(fmt.Sprint(out["why"]), "too few requests") || !strings.Contains(fmt.Sprint(out["why"]), "follow undo") || !strings.Contains(fmt.Sprint(out["undo"]), "revert instance="+first) || fleetKnob.Get(held.Snapshot()) != 2 {
			t.Fatalf("rollout = %v; %s holds %d; want stopped, unjudged, the change kept with how to revert it", out, first, fleetKnob.Get(held.Snapshot()))
		}
	})
}

// The rest of the fleet in an impact report is gathered as the fleet view
// is: a member too busy to answer is taken from its gossip, not dropped, and
// the same members are asked after as before.
func TestTheRestIncludesAMemberTooBusyToAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(int) time.Duration { return 10 * time.Millisecond }, a, b, c)
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		vis.StallSummaries(c, time.Hour)
		rec, err := a.Apply(vis.Tx{By: claude, For: 10 * time.Minute, Reason: "x", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 3}}})
		if err != nil {
			t.Fatal(err)
		}
		im, err := a.WaitImpact(ctx, rec.Change)
		if err != nil {
			t.Fatal(err)
		}
		if im.Rest == nil || len(im.Rest.Members) != 2 || !slices.Contains(im.Rest.Gossiped, "c") {
			t.Fatalf("rest = %+v, want b and c, c from its gossip", im.Rest)
		}
		vis.StallSummaries(c, 0)
	})
}

// When the rest of the fleet was changed too during the windows (an agent
// rolling out by hand), the comparison names those changes instead of
// guessing at something they share.
func TestTheComparisonNamesTheRestsOwnChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() { cancel(); load.Wait(); stopA(); stopB(); synctest.Wait() }()
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v <= 2 {
				return 10 * time.Millisecond
			}
			return 200 * time.Millisecond
		}, a, b)
		time.Sleep(65 * time.Second)
		rec, err := a.Apply(vis.Tx{By: claude, For: 10 * time.Minute, Reason: "first", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 2}}})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		if _, err := b.Apply(vis.Tx{By: claude, For: 10 * time.Minute, Reason: "and the rest", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 2}}}); err != nil {
			t.Fatal(err)
		}
		im, err := a.WaitImpact(ctx, rec.Change)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(im.Summary, "changed too") || !strings.Contains(im.Summary, "b: fleet_test_knob") || strings.Contains(im.Summary, "something they share") {
			t.Fatalf("summary = %q, want b's own change named, not a guess", im.Summary)
		}
	})
}

// Each member says when it was last changed, and how: a fix that lapsed, or
// a change that made a member sick, starts there.
func TestEachMemberSaysWhenItWasLastChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 9, By: sam}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Minute)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "fleet", nil)
		if isErr {
			t.Fatalf("fleet: %v", out["error"])
		}
		for _, m := range out["members"].([]any) {
			m := m.(map[string]any)
			switch last := fmt.Sprint(m["last_change"]); m["name"] {
			case "b":
				if !strings.Contains(last, "fleet_test_knob 5 → 9 by human:sam") || !strings.Contains(last, "3m ago") {
					t.Errorf("b's last change = %q; want fleet_test_knob 5 → 9 by human:sam, 3m ago", last)
				}
			case "a":
				if m["last_change"] != nil {
					t.Errorf("a was never changed, yet its last change = %q", last)
				}
			}
		}
	})
}

// While c crash-looped, the fleet read "4 of 4" for seconds after each
// restart: the new c and its dead former self, until the former went quiet
// long enough to be called replaced. A name with a fresh member lists only it.
func TestARestartedMemberIsListedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b)
		stopB()
		time.Sleep(time.Second)
		_, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stop(); synctest.Wait() }()
		time.Sleep(2500 * time.Millisecond) // the new b is heard; the former, quiet 3.5 s, isn't yet called replaced
		if got := members(a); len(got) != 2 {
			t.Fatalf("a lists %v, want a and b", got)
		}
	})
}
