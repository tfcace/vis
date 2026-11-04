package vis_test

// These tests came from a second review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// three starts a, b, c with an SLO and a load whose latency follows the knob.
func three(t *testing.T, lat func(int) (time.Duration, int), extra ...func(name string) []any) (map[string]*vis.Instance, *pipenet.Network, func()) {
	n := pipenet.New()
	slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
	ins := map[string]*vis.Instance{}
	var stops []func()
	for _, name := range []string{"a", "b", "c"} {
		seed := "a:7946"
		if name == "a" {
			seed = ""
		}
		opts := []any{slo}
		for _, e := range extra {
			opts = append(opts, e(name)...)
		}
		in, stop := peer(t, n, "cacheproxy", name, "k", seed, opts...)
		ins[name] = in
		stops = append(stops, stop)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, in := range ins {
		wg.Go(func() {
			for ctx.Err() == nil {
				_, end := in.StartRequest(ctx)
				d, code := lat(fleetKnob.Get(in.Snapshot()))
				time.Sleep(d)
				end("GET /fleet/{k}", code)
			}
		})
	}
	return ins, n, func() {
		cancel()
		wg.Wait()
		for _, s := range stops {
			s()
		}
		synctest.Wait()
	}
}

func values(ins map[string]*vis.Instance) string {
	return fmt.Sprintf("a=%d b=%d c=%d", fleetKnob.Get(ins["a"].Snapshot()), fleetKnob.Get(ins["b"].Snapshot()), fleetKnob.Get(ins["c"].Snapshot()))
}

func slow9(v int) (time.Duration, int) {
	if v >= 9 {
		return 300 * time.Millisecond, 200
	}
	return 10 * time.Millisecond, 200
}

// R1: a freeze arrives while stage 1 is measured; the undo is refused, and the result says undone anyway.
func TestAnUndoRefusedUnderAFreezeIsReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, slow9)
		defer stop()
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		go func() {
			time.Sleep(8 * time.Second)
			ins["a"].Freeze(vis.Writer{Kind: vis.Agent, ID: "watcher"}, "p99 is up on "+first)
		}()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"})
		t.Logf("first=%s outcome=%v why=%v; %s", first, out["outcome"], out["why"], values(ins))
		if strings.Contains(fmt.Sprint(out["why"]), "undone on") && fleetKnob.Get(ins[first].Snapshot()) == 9 {
			t.Errorf("the rollout says %q but %s still holds 9", out["why"], first)
		}
	})
}

// R2: stage 1 skips a masked member but leaves its lease; when the pin ends, the stopped rollout's value takes effect there.
func TestAStoppedRolloutUndoesMaskedMembersToo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, slow9)
		defer stop()
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		if _, err := ins[first].Set(vis.Change{Knob: "fleet_test_knob", Value: 5, By: sam, For: 2 * time.Minute, Reason: "hold at 5 for a bit"}); err != nil {
			t.Fatal(err)
		}
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"})
		t.Logf("masked=%s outcome=%v why=%v; %s", first, out["outcome"], out["why"], values(ins))
		time.Sleep(3 * time.Minute)
		st, _ := ins[first].Stack("fleet_test_knob")
		t.Logf("after the pin ended: %s; %s's top layer by %v", values(ins), first, st[0].By)
		if out["outcome"] == "stopped" && fleetKnob.Get(ins[first].Snapshot()) == 9 {
			t.Errorf("a stopped rollout's value took effect on %s, unmeasured, when the pin ended", first)
		}
	})
}

// R3: running the same rollout again (to reach a member the first missed, or one a restart dropped) fails at stage 1.
func TestARepeatedRolloutReachesWhatItMissed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, func(v int) (time.Duration, int) {
			if v <= 2 {
				return 10 * time.Millisecond, 200
			}
			return 200 * time.Millisecond, 200
		})
		defer stop()
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fast"})
		if out["outcome"] != "done" {
			t.Fatalf("first rollout: %v", out)
		}
		// The member last in the order loses the value, as a restart would.
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		last := order[2]
		st, _ := ins[last].Stack("fleet_test_knob")
		_ = st
		ls := lastLease(t, cs, last)
		if _, isErr := call(t, cs, "revert", map[string]any{"instance": last, "lease": ls, "reason": "lost"}); isErr {
			t.Fatal("revert")
		}
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		out, _ = call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "again, for " + last})
		t.Logf("again: outcome=%v why=%v; %s", out["outcome"], out["why"], values(ins))
		if out["outcome"] != "done" {
			t.Errorf("the repeated rollout = %v, %v; %s never gets the value", out["outcome"], out["why"], last)
		}
	})
}

func lastLease(t *testing.T, cs *mcp.ClientSession, member string) any {
	k, _ := call(t, cs, "knobs", map[string]any{"instance": member, "knob": "fleet_test_knob"})
	b, _ := json.Marshal(k)
	var v struct {
		Knobs []struct {
			Stack []struct {
				Lease any `json:"lease"`
			} `json:"stack"`
		} `json:"knobs"`
	}
	json.Unmarshal(b, &v)
	for _, l := range v.Knobs[0].Stack {
		if l.Lease != nil && fmt.Sprint(l.Lease) != "0" {
			return l.Lease
		}
	}
	t.Fatalf("no lease in %s", b)
	return nil
}

// R4: a lease shorter than the settle time is judged on a window where it had already ended.
func TestARolloutRefusesALeaseShorterThanItsMeasurement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, slow9)
		defer stop()
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "5s", "reason": "try 9 briefly"})
		if isErr {
			t.Logf("refused: %v", out["error"])
			return
		}
		s1 := out["stages"].([]any)[0].(map[string]any)
		t.Logf("outcome=%v why=%v evidence=%v; %s", out["outcome"], out["why"], s1["evidence"], values(ins))
		if out["outcome"] == "done" {
			t.Errorf("a change that makes p99 30x worse rolled out to the fleet: judged after its 5 s lease ended")
		}
	})
}

// R5: a change that makes the first member fail fast lowers its p99 and passes the gate.
func TestFastErrorsStopARollout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, func(v int) (time.Duration, int) {
			if v >= 9 {
				return time.Millisecond, 503
			}
			return 10 * time.Millisecond, 200
		})
		defer stop()
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"})
		s1 := out["stages"].([]any)[0].(map[string]any)
		t.Logf("outcome=%v evidence=%v; %s", out["outcome"], s1["evidence"], values(ins))
		if out["outcome"] == "done" {
			t.Errorf("every request on stage 1 failed, and the rollout went on to the whole fleet")
		}
	})
}

// R6: the caller gives up during the 30 s measurement; stage 1 stays changed, never judged nor undone.
func TestACallerThatGivesUpDoesntStrandStageOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, slow9)
		defer stop()
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rollout", Arguments: map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"}})
		cancel()
		time.Sleep(60 * time.Second)
		var hs []string
		for _, e := range ins[first].History(50) {
			if e.Kind == "change" {
				hs = append(hs, fmt.Sprint(e.Fields["op"], " ", e.Fields["new"], " ", e.Fields["reason"]))
			}
		}
		t.Logf("client err=%v; a minute later %s; %s history %v", err, values(ins), first, hs)
		if fleetKnob.Get(ins[first].Snapshot()) == 9 {
			t.Errorf("%s holds the harmful 9 with nobody told: the gate never ran", first)
		}
	})
}

// R7: a person forgets a member that is only cut off for a moment; once back, the fleet can't see it for 10 minutes.
func TestAForgottenMemberThatWasOnlyCutOffComesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		var off atomic.Bool
		dial := func(from string) vis.ServeOption {
			return vis.PeerDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
				if off.Load() && (from == "c" || strings.HasPrefix(addr, "c:")) {
					return nil, &net.OpError{Op: "dial", Net: "pipe", Err: errors.New("partitioned")}
				}
				conn, err := n.Dial(ctx, network, addr)
				if err != nil || !(from == "c" || strings.HasPrefix(addr, "c:")) {
					return conn, err
				}
				return &offConn{Conn: conn, off: &off}, nil
			})
		}
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", dial("a"))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", dial("b"))
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", dial("c"))
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		off.Store(true)
		time.Sleep(16 * time.Second) // dead: a suspect member can't be forgotten
		t.Logf("cut off: a sees %v", members(a))
		if err := a.Forget(sam, "c", "looks gone"); err != nil {
			t.Fatalf("forget: %v", err)
		}
		off.Store(false)
		time.Sleep(time.Minute)
		t.Logf("a minute after it's back: a %v, b %v, c %v", members(a), members(b), members(c))
		t.Logf("a's fleet: %s", a.Fleet(context.Background()).Covers)
		if !slices.Contains(members(a), "c:alive") {
			t.Errorf("c is alive and gossiping, and a can't see it")
		}
	})
}

type offConn struct {
	net.Conn
	off *atomic.Bool
}

func (k *offConn) Write(p []byte) (int, error) {
	if k.off.Load() {
		k.Conn.Close()
		return 0, errors.New("partitioned")
	}
	return k.Conn.Write(p)
}

// The coordinator is stage 1: its own change, measured over the peer port, against the rest.
func TestTheCoordinatorAsStageOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, slow9)
		defer stop()
		time.Sleep(65 * time.Second) // past the first minute, when a change can be judged
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		cs := connectOver(t, n, ins[first], first+":7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"})
		t.Logf("coordinator %s: %v %v %v; %s", first, isErr, out["outcome"], out["why"], values(ins))
		if out["outcome"] != "stopped" || fleetKnob.Get(ins[first].Snapshot()) != 5 {
			t.Errorf("coordinator as stage 1: %v", out)
		}
	})
}

// A first member too starved to answer quickly is still measured: reading its
// impact report is part of the change, and gets a change's patience. And a
// measurement that fails anyway stops the rollout without undoing: it can't
// judge, so the member keeps the change and the result says how to revert it.
func TestAStarvedFirstMemberIsStillMeasured(t *testing.T) {
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
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v <= 2 {
				return 10 * time.Millisecond
			}
			return 200 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second)
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		if first != "a" { // the coordinator's own calls aren't forwarded over the network: stall a peer
			vis.StallPeers(map[string]*vis.Instance{"b": b, "c": c}[first], 6*time.Second)
		}
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "starved fleet"})
		if isErr {
			t.Fatalf("rollout with a starved first member: %v", out)
		}
		if lr := lastRollout(t, cs); lr["outcome"] != "done" { // written past the answer, where members are slow
			t.Fatalf("rollout with a starved first member: %v, then %v", out, lr)
		}
	})
}

// A first member that can't be reached isn't the end of a rollout: stage 1
// moves on to the next, and the unreachable one is tried in stage 2 and named
// if it still can't be reached. Round-20261103-0231 failed two rollouts at
// a starved member's connection.
func TestARolloutPassesOverAnUnreachableFirstMember(t *testing.T) {
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
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v <= 2 {
				return 10 * time.Millisecond
			}
			return 200 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second)
		ins := map[string]*vis.Instance{"a": a, "b": b, "c": c}
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		stuck := order[0]
		if stuck == "a" { // the coordinator's own calls aren't forwarded: use the next
			stuck = order[1]
		}
		vis.StallPeers(ins[stuck], time.Hour)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "one can't be reached"})
		if isErr {
			t.Fatalf("rollout: %v", out["error"])
		}
		if out["outcome"] == "writing" || out["outcome"] == "running" { // it goes on past the answer: how it ended is in fleet's last_rollout
			lr := lastRollout(t, cs)
			out["not_reached"] = fmt.Sprint(lr["why"], lr["not_reached"])
		}
		vis.StallPeers(ins[stuck], 0)
		changed := 0
		for name, in := range ins {
			if name != stuck && fleetKnob.Get(in.Snapshot()) == 2 {
				changed++
			}
		}
		if changed != 2 || !strings.Contains(fmt.Sprint(out["not_reached"]), stuck) {
			t.Fatalf("rollout = %v; want the two others changed and %s named as not reached", out, stuck)
		}
	})
}

// A rollout that is done says what it did: every member's before and after,
// the fleet's, and how to take it all back, so neither the agent nor the
// person has to gather it by hand. In fleet-squeeze every agent followed a
// done rollout with two or three calls to find out.
func TestADoneRolloutSaysWhatItDid(t *testing.T) {
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
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v <= 2 {
				return 10 * time.Millisecond
			}
			return 200 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		start := time.Now()
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "slow everywhere"})
		if isErr || out["outcome"] != "done" {
			t.Fatalf("rollout = %v", out)
		}
		if took := time.Since(start); took > 50*time.Second { // an MCP client gives up on a call at a minute
			t.Errorf("rollout took %v to answer; want it well under a minute", took)
		}
		if u := fmt.Sprint(out["undo"]); strings.Count(u, "revert instance=") != 3 {
			t.Errorf("undo = %q; want one revert per member", u)
		}
		time.Sleep(time.Until(mustTime(t, out["measured_at"])) + time.Second)
		f, _ := call(t, cs, "fleet", nil)
		lr, _ := f["last_rollout"].(map[string]any)
		if lr["state"] != "measured" {
			t.Fatalf("last_rollout = %v; want it measured by measured_at", lr)
		}
		out = lr
		res := fmt.Sprint(out["result"])
		for _, m := range []string{"a:", "b:", "c:"} {
			if !strings.Contains(res, m) {
				t.Errorf("result = %s; want a row for %s", res, m)
			}
		}
		if !strings.Contains(res, "→ 10 ms") {
			t.Errorf("result = %s; want each member's p99 before and after", res)
		}
		if f := fmt.Sprint(out["fleet"]); !strings.Contains(f, "→") || !strings.Contains(f, "meets") {
			t.Errorf("fleet = %q; want the fleet's p99 before and after, and whether it meets its objective", f)
		}
	})
}

// lastRollout waits for fleet's last_rollout to have stage 2 written, and returns it.
func lastRollout(t *testing.T, cs *mcp.ClientSession) map[string]any {
	t.Helper()
	for range 60 {
		f, _ := call(t, cs, "fleet", nil)
		if lr, ok := f["last_rollout"].(map[string]any); ok && lr["state"] != "running" && lr["state"] != "writing stage 2" {
			return lr
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatal("no last_rollout in fleet after 5 minutes")
	return nil
}

func mustTime(t *testing.T, v any) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, fmt.Sprint(v))
	if err != nil {
		t.Fatalf("not a time: %v", v)
	}
	return at
}
