package vis_test

// These tests came from a ninth review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// R1: the first member restarts during the gate. Why says there is nothing to
// revert; undo (now filled on every path) still says revert instance=b lease=1,
// which on the new process is another agent's lease.
func TestARestartedFirstMemberIsNotInUndo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		var stopB2 func()
		var b2 *vis.Instance
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopC()
			if stopB2 != nil {
				stopB2()
			}
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v >= 9 {
				return 300 * time.Millisecond
			}
			return 10 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second)
		first := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")[0]
		if first != "b" {
			t.Skipf("first is %s", first)
		}
		go func() {
			time.Sleep(5 * time.Second)
			vis.SkipLeave(b)
			stopB()
			time.Sleep(time.Second)
			b2, stopB2 = peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
			if _, err := b2.Set(vis.Change{Knob: "fleet_test_knob", Value: 7, By: vis.Writer{Kind: vis.Agent, ID: "watcher"}, For: 10 * time.Minute, Reason: "another agent"}); err != nil {
				t.Error(err)
			}
		}()
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 9}, "for": "10m", "reason": "try 9"})
		t.Logf("outcome=%v\nwhy=%v\nundo=%v", out["outcome"], out["why"], out["undo"])
		time.Sleep(5 * time.Second)
		if !strings.Contains(fmt.Sprint(out["why"]), "restarted") {
			t.Skipf("not the restart path: %v", out["why"])
		}
		if u := fmt.Sprint(out["undo"]); strings.Contains(u, "revert instance=b ") {
			rv, isErr := call(t, cs, "revert", map[string]any{"instance": "b", "lease": 1, "reason": "follow undo"})
			t.Logf("following undo: isErr=%v %v; new b now holds %d", isErr, rv, fleetKnob.Get(b2.Snapshot()))
			t.Errorf("why says b's lease is gone with the old process, but undo says %q: following it reverts the new process's lease", u)
		}
		time.Sleep(time.Minute)
	})
}

// fourMembers starts a, b, c, d with a load whose latency follows the knob.
func fourMembers(t *testing.T) (map[string]*vis.Instance, *pipenet.Network, func()) {
	n := pipenet.New()
	slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
	ins := map[string]*vis.Instance{}
	var stops []func()
	for _, name := range []string{"a", "b", "c", "d"} {
		seed := "a:7946"
		if name == "a" {
			seed = ""
		}
		in, stop := peer(t, n, "cacheproxy", name, "k", seed, slo)
		ins[name], stops = in, append(stops, stop)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var load sync.WaitGroup
	var all []*vis.Instance
	for _, in := range ins {
		all = append(all, in)
	}
	fleetLoad(ctx, &load, func(v int) time.Duration {
		if v <= 2 {
			return 10 * time.Millisecond
		}
		return 200 * time.Millisecond
	}, all...)
	return ins, n, func() {
		cancel()
		load.Wait()
		for _, s := range stops {
			s()
		}
		synctest.Wait()
	}
}

// R2a: one member stalled from the start (passed over in stage 1, not retried
// in stage 2): how long the rollout takes to answer.
func TestARolloutAnswersInAMinuteWithAPassOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, func(v int) (time.Duration, int) {
			if v <= 2 {
				return 10 * time.Millisecond, 200
			}
			return 200 * time.Millisecond, 200
		})
		defer stop()
		time.Sleep(65 * time.Second)
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		vis.StallPeers(ins[order[0]], time.Hour)
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		start := time.Now()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		took := time.Since(start)
		vis.StallPeers(ins[order[0]], 0)
		t.Logf("order=%v coord=%s took=%v outcome=%v why=%v not_reached=%v", order, coord, took, out["outcome"], out["why"], out["not_reached"])
		if took > 55*time.Second {
			t.Errorf("rollout took %v to answer: past what an MCP client waits (60 s, less transport)", took)
		}
		time.Sleep(2 * time.Minute)
	})
}

// R2b: four members; order[0] stalled from the start, order[2] stalled once
// stage 1 is written: stage 2 tries it, and tries again.
func TestARolloutAnswersInAMinuteWithAPassOverAndARetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := fourMembers(t)
		defer stop()
		time.Sleep(65 * time.Second)
		order := vis.StageOrder([]string{"a", "b", "c", "d"}, "fleet_test_knob")
		coord := order[3]
		vis.StallPeers(ins[order[0]], time.Hour)
		go func() {
			time.Sleep(20 * time.Second) // past stage 1's pass-over and write
			vis.StallPeers(ins[order[2]], time.Hour)
		}()
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		start := time.Now()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		took := time.Since(start)
		vis.StallPeers(ins[order[0]], 0)
		vis.StallPeers(ins[order[2]], 0)
		t.Logf("order=%v coord=%s took=%v outcome=%v why=%v not_reached=%v", order, coord, took, out["outcome"], out["why"], out["not_reached"])
		if took > 55*time.Second {
			t.Errorf("rollout took %v to answer: past what an MCP client waits (60 s, less transport)", took)
		}
		time.Sleep(2 * time.Minute)
	})
}

// R2d: the canary's impact report is late past lateMeasure (a published
// signal is slow to read once the change is in, as a starved process is): the
// gate can't measure, then waits for a restart.
func TestARolloutAnswersInAMinuteWithALateImpactReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var slow sync.Map
		ins, n, stop := three(t, func(v int) (time.Duration, int) {
			if v <= 2 {
				return 10 * time.Millisecond, 200
			}
			return 200 * time.Millisecond, 200
		})
		defer stop()
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		canary := ins[order[0]]
		canary.Signal("slow_read", "", "a signal slow to read once the change is in", func() float64 {
			if _, ok := slow.Load("on"); ok && fleetKnob.Get(canary.Snapshot()) == 2 {
				time.Sleep(25 * time.Second)
			}
			return 1
		})
		time.Sleep(65 * time.Second)
		slow.Store("on", true)
		coord := order[len(order)-1]
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		start := time.Now()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		took := time.Since(start)
		slow.Delete("on")
		t.Logf("order=%v coord=%s took=%v outcome=%v why=%v", order, coord, took, out["outcome"], out["why"])
		if took > 55*time.Second {
			t.Errorf("rollout took %v to answer: past what an MCP client waits (60 s, less transport)", took)
		}
		time.Sleep(2 * time.Minute)
	})
}

// A member passed over, and then a canary whose impact report comes late:
// the gate's read is cut at the answer's deadline, which stops the rollout
// and keeps the change, rather than answering past a minute.
func TestARolloutAnswersInAMinuteWithAPassOverAndALateImpactReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var slow sync.Map
		ins, n, stop := three(t, func(v int) (time.Duration, int) {
			if v <= 2 {
				return 10 * time.Millisecond, 200
			}
			return 200 * time.Millisecond, 200
		})
		defer stop()
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		canary := ins[order[1]] // the first is passed over
		canary.Signal("slow_read_2", "", "a signal slow to read once the change is in", func() float64 {
			if _, ok := slow.Load("on"); ok && fleetKnob.Get(canary.Snapshot()) == 2 {
				time.Sleep(25 * time.Second)
			}
			return 1
		})
		time.Sleep(65 * time.Second)
		slow.Store("on", true)
		vis.StallPeers(ins[order[0]], time.Hour)
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		start := time.Now()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		took := time.Since(start)
		vis.StallPeers(ins[order[0]], 0)
		slow.Delete("on")
		if took > 55*time.Second {
			t.Errorf("rollout took %v to answer: past what an MCP client waits (60 s, less transport)", took)
		}
		if out["outcome"] == "running" { // it goes on past the answer: how it ended is in fleet's last_rollout
			out = lastRollout(t, cs)
		}
		if out["outcome"] == "done" || !strings.Contains(fmt.Sprint(out["undo"]), "revert instance="+order[1]) {
			t.Errorf("rollout = %v; want it stopped, keeping the change on %s, with how to take it back", out, order[1])
		}
		time.Sleep(2 * time.Minute)
	})
}

// Every member slow to answer, as on a fleet starved of CPU: the rollout
// still reaches them all, with a member's full patience, answering within a
// minute and saying how it ended in last_rollout. Round-20261103-0530's last
// fleet-relapse run, with handshakes bounded to fit an answer, reached none.
func TestARolloutReachesAFleetSlowToAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, func(v int) (time.Duration, int) {
			if v <= 2 {
				return 10 * time.Millisecond, 200
			}
			return 200 * time.Millisecond, 200
		})
		defer stop()
		time.Sleep(65 * time.Second)
		for _, in := range ins {
			vis.StallCalls(in, 8*time.Second) // every forwarded call to every member waits 8 s; gossip gets through
		}
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		start := time.Now()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "starved"})
		if took := time.Since(start); took > 55*time.Second {
			t.Errorf("rollout took %v to answer", took)
		}
		if out["outcome"] == "running" || out["outcome"] == "writing" {
			for range 30 { // until stage 2 is written
				if lr := lastRollout(t, cs); lr["state"] != nil && lr["outcome"] != nil { // lastRollout skips one still running or writing
					out = lr
					break
				}
				time.Sleep(5 * time.Second)
			}
		}
		for _, in := range ins {
			vis.StallCalls(in, 0)
		}
		for name, in := range ins {
			if fleetKnob.Get(in.Snapshot()) != 2 {
				t.Errorf("%s holds %d; a fleet slow to answer is still reached (rollout: %v)", name, fleetKnob.Get(in.Snapshot()), out)
			}
		}
		time.Sleep(2 * time.Minute)
	})
}
