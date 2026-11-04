package vis_test

// Eleventh review: cross-feature scenarios against the fleet's promises.

// These tests came from an eleventh review of the fleet layer, a functional audit across features, each a reproducer of a defect it found.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

func fastAt2(v int) time.Duration {
	if v <= 2 {
		return 10 * time.Millisecond
	}
	return 200 * time.Millisecond
}

// A: the rollout's answer says fleet shows how it ends; fleet asked of any
// other member (a load balancer, a reconnect, another session) has no
// last_rollout, and fleet takes no instance to ask the coordinator.
func TestEveryMemberShowsTheLastRollout(t *testing.T) {
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
		fleetLoad(ctx, &load, fastAt2, a, b, c)
		time.Sleep(65 * time.Second)
		ca := connectOver(t, n, a, "a:7070", "oncall-secret")
		cb := connectOver(t, n, b, "b:7070", "oncall-secret")
		out, _ := call(t, ca, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "slow"})
		t.Logf("rollout via a: outcome=%v note=%v", out["outcome"], out["note"])
		time.Sleep(time.Until(mustTime(t, out["measured_at"])) + 2*time.Second)
		fa, _ := call(t, ca, "fleet", nil)
		fb, _ := call(t, cb, "fleet", nil)
		t.Logf("fleet via a: last_rollout state=%v", fa["last_rollout"].(map[string]any)["state"])
		t.Logf("fleet via b: last_rollout=%v", fb["last_rollout"])
		if fb["last_rollout"] == nil {
			t.Errorf("fleet asked of b has no last_rollout; a's rollout changed b too")
		}
		// fleet takes no instance: the agent can't ask a through b
		fi, isErr := call(t, cb, "fleet", map[string]any{"instance": "a"})
		t.Logf("fleet instance=a via b: isErr=%v last_rollout=%v", isErr, fi["last_rollout"])
	})
}

// B: a second rollout through another member while the first runs is not
// refused: two coordinators at once, same set.
func TestASecondCoordinatorIsRefusedWhileARolloutRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2)
		defer stop()
		time.Sleep(65 * time.Second)
		for _, in := range ins {
			vis.StallCalls(in, 8*time.Second)
		}
		ca := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		cb := connectOver(t, n, ins["b"], "b:7070", "oncall-secret")
		set := map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fix"}
		start := time.Now()
		out1, _ := call(t, ca, "rollout", set)
		t.Logf("rollout 1 via a after %v: outcome=%v why=%v", time.Since(start), out1["outcome"], out1["why"])
		out2, isErr := call(t, cb, "rollout", set)
		t.Logf("rollout 2 via b: isErr=%v outcome=%v why=%v undo=%v err=%v", isErr, out2["outcome"], out2["why"], out2["undo"], out2["error"])
		if out1["outcome"] == "running" && !isErr {
			t.Errorf("a second rollout of the same set through b ran while a's still ran")
		}
		for _, in := range ins {
			vis.StallCalls(in, 0)
		}
		time.Sleep(3 * time.Minute)
		fa, _ := call(t, ca, "fleet", nil)
		fb, _ := call(t, cb, "fleet", nil)
		t.Logf("a's last_rollout: %v", fa["last_rollout"])
		t.Logf("b's last_rollout: %v", fb["last_rollout"])
		t.Logf("values %s", values(ins))
		lr, _ := fa["last_rollout"].(map[string]any)
		re := regexp.MustCompile(`revert instance=(\w+) lease=(\d+) process=(\w+)`)
		for _, m := range re.FindAllStringSubmatch(fmt.Sprint(lr["undo"]), -1) {
			var lease int
			fmt.Sscan(m[2], &lease)
			r, isErr := call(t, ca, "revert", map[string]any{"instance": m[1], "lease": lease, "process": m[3], "reason": "undo rollout 1"})
			t.Logf("revert %s lease %d: isErr=%v %v", m[1], lease, isErr, r)
		}
		t.Logf("after following a's last_rollout undo: %s", values(ins))
		if values(ins) != "a=5 b=5 c=5" {
			t.Errorf("following a's last_rollout undo left %s", values(ins))
		}
	})
}

// C: a member restarts after a done rollout; another agent changes it; the
// rollout's undo (fleet's last_rollout) then reverts that agent's change.
func TestUndoAfterAMemberRestartedDoesntRevertAnothersChange(t *testing.T) {
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
		fleetLoad(ctx, &load, fastAt2, a, b, c)
		time.Sleep(65 * time.Second)
		ca := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, _ := call(t, ca, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "slow"})
		if out["outcome"] != "done" {
			t.Fatalf("rollout: %v %v", out["outcome"], out["why"])
		}
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		victim := order[2]
		if victim == "a" {
			victim = order[1]
		}
		ins := map[string]*vis.Instance{"a": a, "b": b, "c": c}
		stops := map[string]*func(){"b": &stopB, "c": &stopC}
		vis.SkipLeave(ins[victim])
		(*stops[victim])()
		time.Sleep(2 * time.Second)
		v2, stopV2 := peer(t, n, "cacheproxy", victim, "k", "a:7946", slo)
		*stops[victim] = stopV2
		ins[victim] = v2
		fleetLoad(ctx, &load, fastAt2, v2)
		if _, err := v2.Set(vis.Change{Knob: "fleet_test_knob", Value: 1, By: vis.Writer{Kind: vis.Agent, ID: "watcher"}, For: 10 * time.Minute, Reason: "another agent's own fix"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Second)
		f, _ := call(t, ca, "fleet", nil)
		lr, _ := f["last_rollout"].(map[string]any)
		t.Logf("last_rollout undo after %s restarted: %v", victim, lr["undo"])
		re := regexp.MustCompile(`revert instance=(\w+) lease=(\d+) process=(\w+)`)
		for _, m := range re.FindAllStringSubmatch(fmt.Sprint(lr["undo"]), -1) {
			var lease int
			fmt.Sscan(m[2], &lease)
			r, isErr := call(t, ca, "revert", map[string]any{"instance": m[1], "lease": lease, "process": m[3], "reason": "undo the rollout"})
			t.Logf("revert %s lease %d: isErr=%v %v", m[1], lease, isErr, r)
		}
		st, _ := v2.Stack("fleet_test_knob")
		t.Logf("%s now: %d, top layer %v by %v", victim, fleetKnob.Get(v2.Snapshot()), st[0].Layer, st[0].By)
		if fleetKnob.Get(v2.Snapshot()) != 1 {
			t.Errorf("following last_rollout's undo reverted watcher's change on the restarted %s", victim)
		}
	})
}

// D: a person's rollout during a freeze: the freeze says it stops rollouts.
func TestAPersonsRolloutIsRefusedDuringAFreeze(t *testing.T) {
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
		fleetLoad(ctx, &load, fastAt2, a, b, c)
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "sam-secret")
		a.Freeze(claude, "investigating")
		time.Sleep(3 * time.Second)
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "sam's rollout"})
		t.Logf("sam's rollout while frozen: isErr=%v outcome=%v why=%v stages=%v err=%v", isErr, out["outcome"], out["why"], out["stages"], out["error"])
		if !isErr || !strings.Contains(fmt.Sprint(out["error"]), "frozen") || fleetKnob.Get(b.Snapshot()) != 5 || fleetKnob.Get(c.Snapshot()) != 5 {
			t.Errorf("a person's rollout while the fleet is frozen: %v; values a=%d b=%d c=%d; want it refused, nothing changed", out, fleetKnob.Get(a.Snapshot()), fleetKnob.Get(b.Snapshot()), fleetKnob.Get(c.Snapshot()))
		}
	})
}

// E: a fleet of one (a ring with no other member).
func TestARolloutOnAFleetOfOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() { cancel(); load.Wait(); stopA(); synctest.Wait() }()
		fleetLoad(ctx, &load, fastAt2, a)
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		f, _ := call(t, cs, "fleet", nil)
		t.Logf("fleet: covers=%v slos=%v standout=%v common=%v missing=%v", f["covers"], f["slos"], f["stand_out"], f["common"], f["missing"])
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "slow"})
		t.Logf("rollout: isErr=%v outcome=%v why=%v stages=%v undo=%v note=%v measured_at=%v", isErr, out["outcome"], out["why"], out["stages"], out["undo"], out["note"], out["measured_at"])
		if ma, ok := out["measured_at"]; ok && ma != nil {
			time.Sleep(time.Until(mustTime(t, ma)) + 2*time.Second)
		} else {
			time.Sleep(time.Minute)
		}
		f, _ = call(t, cs, "fleet", nil)
		t.Logf("last_rollout: %v", f["last_rollout"])
	})
}

// F: impact named at a starved member, right after set_knob there, as its
// description says to: how long the call takes, against the client's 60 s.
func TestImpactOnAStarvedMemberAnswersInAMinute(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("late=", late), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var slow sync.Map
				ins, n, stop := three(t, fast2)
				defer stop()
				b := ins["b"]
				b.Signal("slow_read_f", "", "slow to read once the change is in", func() float64 {
					if _, ok := slow.Load("on"); ok && fleetKnob.Get(b.Snapshot()) == 2 {
						time.Sleep(25 * time.Second)
					}
					return 1
				})
				time.Sleep(65 * time.Second)
				if late {
					slow.Store("on", true)
				}
				vis.StallCalls(b, 7*time.Second)
				cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
				start := time.Now()
				out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "b", "set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fix b"})
				t.Logf("set_knob took %v isErr=%v change=%v", time.Since(start), isErr, out["change"])
				start = time.Now()
				im, isErr := call(t, cs, "impact", map[string]any{"instance": "b", "change": out["change"]})
				took := time.Since(start)
				t.Logf("impact took %v isErr=%v summary=%v err=%v", took, isErr, im["summary"], im["error"])
				if took >= 60*time.Second {
					t.Errorf("impact on b took %v: an MCP client cut it at 60 s", took)
				}
				slow.Delete("on")
				vis.StallCalls(b, 0)
				time.Sleep(time.Minute)
			})
		})
	}
}

// peerAt is peer with a name apart from its address, as processes on one
// host that share the hostname (VIS_INSTANCE_NAME's default) would be.
func peerAt(t *testing.T, n *pipenet.Network, name, addr, seed string, extra ...vis.InstanceOption) (*vis.Instance, func()) {
	ln, err := n.Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	in := vis.NewInstance(append([]vis.InstanceOption{vis.Service("cacheproxy"), vis.InstanceName(name)}, extra...)...)
	ctx, cancel := context.WithCancel(context.Background())
	opts := []vis.ServeOption{vis.MCPAt("", tokens), vis.UIAt(""), vis.PeerListener(ln), vis.PeerDialer(n.Dial), vis.PeerAdvertise(addr), vis.ClusterKey("k")}
	if seed != "" {
		opts = append(opts, vis.Seeds(seed))
	}
	if _, err := in.Serve(ctx, opts...); err != nil {
		t.Fatal(err)
	}
	return in, cancel
}

// G: three processes on one host, each with its own ports, VIS_INSTANCE_NAME unset.
func TestProcessesSharingANameAreNamedAndRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, s1 := peerAt(t, n, "host", "h:7946", "", slo)
		b, s2 := peerAt(t, n, "host", "h:7947", "h:7946", slo)
		c, s3 := peerAt(t, n, "host", "h:7948", "h:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			s1()
			s2()
			s3()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, fastAt2, a, b, c)
		time.Sleep(65 * time.Second)
		for _, in := range []*vis.Instance{a, b, c} {
			t.Logf("members: %v; warnings: %v", members(in), in.Warnings())
		}
		cs := connectOver(t, n, a, "h:7070", "oncall-secret")
		f, _ := call(t, cs, "fleet", nil)
		t.Logf("fleet covers=%v slos=%v stand_out=%v", f["covers"], f["slos"], f["stand_out"])
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "slow"})
		t.Logf("rollout: outcome=%v why=%v not_reached=%v undo=%v", out["outcome"], out["why"], out["not_reached"], out["undo"])
		got := fmt.Sprint(fleetKnob.Get(a.Snapshot()), fleetKnob.Get(b.Snapshot()), fleetKnob.Get(c.Snapshot()))
		t.Logf("values: %s", got)
		time.Sleep(5 * time.Second)
		f, _ = call(t, cs, "fleet", nil)
		t.Logf("fleet after: differ=%v common=%v stand_out=%v", f["differ"], f["common"], f["stand_out"])
		if out["outcome"] == "done" && got != "2 2 2" {
			t.Errorf("rollout says done, but the processes hold %s", got)
		}
	})
}

// H: the coordinator restarts after answering "running"; fleet is then asked
// of another member, and of the coordinator's new process.
func TestACoordinatorRestartMidRollout(t *testing.T) {
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
		fleetLoad(ctx, &load, fastAt2, a, b, c)
		time.Sleep(65 * time.Second)
		ins := map[string]*vis.Instance{"a": a, "b": b, "c": c}
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		for _, in := range ins {
			vis.StallCalls(in, 8*time.Second)
		}
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fix"})
		t.Logf("rollout via %s: outcome=%v why=%v", coord, out["outcome"], out["why"])
		vis.SkipLeave(ins[coord])
		map[string]func(){"a": stopA, "b": stopB, "c": stopC}[coord]()
		time.Sleep(2 * time.Second)
		seed := "a:7946"
		if coord == "a" {
			seed = "b:7946"
		}
		c2, stop2 := peer(t, n, "cacheproxy", coord, "k", seed, slo)
		defer stop2()
		fleetLoad(ctx, &load, fastAt2, c2)
		for _, in := range ins {
			vis.StallCalls(in, 0)
		}
		time.Sleep(90 * time.Second)
		other := order[0]
		co := connectOver(t, n, ins[other], other+":7071", "oncall-secret")
		cn := connectOver(t, n, c2, coord+":7072", "oncall-secret")
		fo, _ := call(t, co, "fleet", nil)
		fn, _ := call(t, cn, "fleet", nil)
		t.Logf("fleet via %s: last_rollout=%v differ=%v", other, fo["last_rollout"], fo["differ"])
		t.Logf("fleet via new %s: last_rollout=%v", coord, fn["last_rollout"])
		for who, f := range map[string]map[string]any{other: fo, "new " + coord: fn} {
			if lr, _ := f["last_rollout"].(map[string]any); !strings.Contains(fmt.Sprint(lr["state"]), "restarted before it ended") {
				t.Errorf("fleet via %s after the coordinator restarted mid-rollout: last_rollout %v; want it said the rollout's end is unknown, and where to look", who, lr)
			}
		}
		for _, m := range fo["members"].([]any) {
			mm := m.(map[string]any)
			t.Logf("  member %v restarts=%v lost=%v held=%v", mm["name"], mm["restarts"], mm["lost"], mm["held"])
		}
		t.Logf("values: a=%d b=%d c=%d new %s=%d", fleetKnob.Get(a.Snapshot()), fleetKnob.Get(b.Snapshot()), fleetKnob.Get(c.Snapshot()), coord, fleetKnob.Get(c2.Snapshot()))
	})
}

// I: a freeze lands while stage 1 is measured; harm, then the undo is refused.
// What the answer says about how long the harm stays, against the freeze.
// And one that passes the gate: what stage 2 says.
func TestAFreezeDuringTheGate(t *testing.T) {
	for _, harmful := range []bool{true, false} {
		t.Run(fmt.Sprint("harmful=", harmful), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lat := slow9
				v := 9
				if !harmful {
					lat, v = fast2, 2
				}
				ins, n, stop := three(t, lat)
				defer stop()
				time.Sleep(65 * time.Second)
				_ = vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
				cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
				go func() {
					time.Sleep(8 * time.Second)
					ins["a"].Freeze(sam, "investigating")
				}()
				out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": v}, "for": "10m", "reason": "try"})
				if why := fmt.Sprint(out["why"]); harmful && strings.Contains(why, "undo failed") && (strings.Contains(why, "until its lease ends") || !strings.Contains(why, "freeze")) {
					t.Errorf("a harmful first stage the freeze kept from being undone: why %q; want it to say the freeze stops the lease's clock, and that a person can take it back", why)
				}
				time.Sleep(15 * time.Minute)
				t.Logf("15 min later, still frozen: %s", values(ins))
				ins["a"].Unfreeze(sam, "done")
				time.Sleep(5 * time.Minute)
				t.Logf("5 min after unfreeze: %s", values(ins))
			})
		})
	}
}

// J: VIS_FLEET_WAIT raised, as doc.go advises where members answer slowly by
// design; a member slow to answer: how long instance= calls take.
func TestARaisedFleetWaitStaysUnderTheMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2, func(string) []any { return []any{vis.FleetWait(30 * time.Second)} })
		defer stop()
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		vis.StallCalls(ins["b"], 35*time.Second)
		vis.StallSummaries(ins["b"], 25*time.Second)
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"describe", map[string]any{"instance": "b"}},
			{"fleet", nil},
			{"set_knob", map[string]any{"instance": "b", "set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fix b"}},
		} {
			start := time.Now()
			out, isErr := call(t, cs, tc.tool, tc.args)
			took := time.Since(start)
			e := fmt.Sprint(out["error"])
			if len(e) > 200 {
				e = e[:200]
			}
			t.Logf("%s took %v isErr=%v err=%s", tc.tool, took, isErr, e)
			if took >= 60*time.Second {
				t.Errorf("%s took %v: an MCP client cuts it at 60 s", tc.tool, took)
			}
			if tc.tool == "set_knob" {
				t.Logf("b holds %d", fleetKnob.Get(ins["b"].Snapshot()))
				start = time.Now()
				im, _ := call(t, cs, "impact", map[string]any{"instance": "b", "change": out["change"]})
				t.Logf("impact took %v: %v", time.Since(start), im["summary"])
			}
		}
		vis.StallCalls(ins["b"], 0)
		vis.StallSummaries(ins["b"], 0)
	})
}

// K: five members, two starved (slow past a change's patience, and too busy
// for the fleet's question): fleet, then a rollout.
func TestFiveWithTwoStarved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		names := []string{"a", "b", "c", "d", "e"}
		ins := map[string]*vis.Instance{}
		var stops []func()
		for _, nm := range names {
			seed := "a:7946"
			if nm == "a" {
				seed = ""
			}
			in, s := peer(t, n, "cacheproxy", nm, "k", seed, slo)
			ins[nm], stops = in, append(stops, s)
		}
		order := vis.StageOrder(names, "fleet_test_knob")
		starved := map[string]bool{order[1]: true, order[3]: true}
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			for _, s := range stops {
				s()
			}
			synctest.Wait()
		}()
		for _, nm := range names {
			lat := func(v int) time.Duration { return 10 * time.Millisecond }
			if starved[nm] {
				lat = fastAt2
			}
			fleetLoad(ctx, &load, lat, ins[nm])
		}
		time.Sleep(65 * time.Second)
		for nm := range starved {
			vis.StallCalls(ins[nm], 20*time.Second)
			vis.StallSummaries(ins[nm], 5*time.Second)
		}
		coord := order[4]
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		f, _ := call(t, cs, "fleet", nil)
		t.Logf("order=%v starved=%v", order, starved)
		t.Logf("fleet covers=%v slos=%v", f["covers"], f["slos"])
		for _, s := range f["stand_out"].([]any) {
			t.Logf("  stand_out %v", s)
		}
		start := time.Now()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fix the starved"})
		t.Logf("rollout after %v: outcome=%v\n why=%v\n undo=%v\n not_reached=%v", time.Since(start), out["outcome"], out["why"], out["undo"], out["not_reached"])
		for _, st := range out["stages"].([]any) {
			t.Logf("  stage %v evidence=%v", st.(map[string]any)["members"], st.(map[string]any)["evidence"])
		}
		lr := lastRollout(t, cs)
		t.Logf("last_rollout: %v", lr)
		var vals []string
		for _, nm := range names {
			vals = append(vals, fmt.Sprintf("%s=%d", nm, fleetKnob.Get(ins[nm].Snapshot())))
		}
		t.Logf("values %v", vals)
		for nm := range starved {
			vis.StallCalls(ins[nm], 0)
			vis.StallSummaries(ins[nm], 0)
		}
	})
}
