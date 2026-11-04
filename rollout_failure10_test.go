package vis_test

// These tests came from a tenth review of the fleet layer, each a reproducer of a defect it found.

import (
	"context"
	"strings"

	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"regexp"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

func fast2(v int) (time.Duration, int) {
	if v <= 2 {
		return 10 * time.Millisecond, 200
	}
	return 200 * time.Millisecond, 200
}

// RV1: after "running", last_rollout still shows the previous rollout of the
// same set, which reads as this one's end. Then a retry while it runs.
func TestARetryWhileARolloutRunsIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2)
		defer stop()
		time.Sleep(65 * time.Second)
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		set := map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "2m", "reason": "fix"}
		out1, _ := call(t, cs, "rollout", set)
		t.Logf("rollout 1: outcome=%v", out1["outcome"])
		time.Sleep(4 * time.Minute) // measured, then every lease ends: the fix lapses everywhere
		t.Logf("after lapse: %s", values(ins))
		f, _ := call(t, cs, "fleet", nil)
		t.Logf("fleet common: %v", f["common"])

		for _, in := range ins {
			vis.StallCalls(in, 8*time.Second)
		}
		set["for"] = "10m"
		start := time.Now()
		out2, _ := call(t, cs, "rollout", set)
		t.Logf("rollout 2 after %v: outcome=%v undo=%q", time.Since(start), out2["outcome"], out2["undo"])
		f, _ = call(t, cs, "fleet", nil)
		lr, _ := f["last_rollout"].(map[string]any)
		if out2["outcome"] != "done" && lr["state"] == "measured" {
			t.Errorf("right after rollout 2 answered %v, last_rollout is the previous rollout's end: %v", out2["outcome"], lr)
		}

		// the agent retries
		start = time.Now()
		out3, isErr := call(t, cs, "rollout", set)
		if out2["outcome"] != "done" && (!isErr || !strings.Contains(fmt.Sprint(out3["error"]), "still running")) {
			t.Errorf("a retry while rollout 2 still runs: %v; want it refused, saying the first is still running", out3)
		}
		for i := range 20 {
			time.Sleep(15 * time.Second)
			f, _ = call(t, cs, "fleet", nil)
			lr, _ = f["last_rollout"].(map[string]any)
			t.Logf("t+%ds last_rollout: outcome=%v state=%v at=%v why=%v undo=%q", (i+1)*15, lr["outcome"], lr["state"], lr["at"], lr["why"], lr["undo"])
		}
		for _, in := range ins {
			vis.StallCalls(in, 0)
		}
		t.Logf("values: %s", values(ins))
		// follow the final report's undo
		re := regexp.MustCompile(`revert instance=(\w+) lease=(\d+) process=(\w+)`)
		for _, m := range re.FindAllStringSubmatch(fmt.Sprint(lr["undo"]), -1) {
			var lease int
			fmt.Sscan(m[2], &lease)
			r, isErr := call(t, cs, "revert", map[string]any{"instance": m[1], "lease": lease, "process": m[3], "reason": "undo"})
			t.Logf("revert %s lease %d: err=%v %v", m[1], lease, isErr, r["error"])
		}
		t.Logf("after following undo: %s", values(ins))
		for _, in := range ins {
			if fleetKnob.Get(in.Snapshot()) != 5 {
				t.Errorf("after following last_rollout's undo: %s; want every member back at 5", values(ins))
				break
			}
		}
		time.Sleep(11 * time.Minute)
	})
}

// RV2: a member down when the rollout starts is in the answer's not_reached;
// after "running" or "writing", is it in last_rollout?
func TestTheReportAfterTheAnswerNamesWhoWasntReached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2)
		defer stop()
		time.Sleep(65 * time.Second)
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		down := order[1]
		vis.SkipLeave(ins[down])
		ins[down].Stop()
		time.Sleep(20 * time.Second)
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		m := ins[coord].Members()
		t.Logf("members: %v", m)
		for name, in := range ins {
			if name != down {
				vis.StallCalls(in, 8*time.Second)
			}
		}
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		t.Logf("answer: outcome=%v not_reached=%v why=%v", out["outcome"], out["not_reached"], out["why"])
		if out["outcome"] == "running" || out["outcome"] == "writing" {
			time.Sleep(2 * time.Minute)
			lr := lastRollout(t, cs)
			t.Logf("last_rollout: %v", lr)
			if !regexp.MustCompile(down).MatchString(fmt.Sprint(lr["not_reached"], lr["why"])) {
				t.Errorf("last_rollout doesn't name %s, down when the rollout started, as not reached: %v", down, lr)
			}
		}
		for name, in := range ins {
			if name != down {
				vis.StallCalls(in, 0)
			}
		}
		time.Sleep(11 * time.Minute)
	})
}

// RV3: stage 1 passed, stage 2 still writing at the deadline: is the answer
// "writing" (stage 1's undo) or "running" (nothing)?
func TestAStage2StillWritingAnswersWithStage1sUndo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2)
		defer stop()
		time.Sleep(65 * time.Second)
		for _, in := range ins {
			vis.StallCalls(in, 8*time.Second)
		}
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		_ = time.Now()
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		if out["outcome"] != "writing" || !strings.Contains(fmt.Sprint(out["undo"]), "revert instance=") {
			t.Errorf("stage 1 passed, stage 2 still writing at the deadline: answer %v, undo %q; want writing, with stage 1's undo", out["outcome"], out["undo"])
		}
		time.Sleep(3 * time.Minute)
		for _, in := range ins {
			vis.StallCalls(in, 0)
		}
		time.Sleep(11 * time.Minute)
	})
}

// RV4: the coordinator stops while stage 1 is measured.
func TestStoppingWhileTheCanaryIsMeasuredDoesntWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2)
		defer stop()
		time.Sleep(65 * time.Second)
		for _, in := range ins {
			vis.StallCalls(in, 8*time.Second)
		}
		order := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		coord := order[len(order)-1]
		cs := connectOver(t, n, ins[coord], coord+":7070", "oncall-secret")
		go cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "rollout", Arguments: map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"}})
		time.Sleep(45 * time.Second)
		t.Logf("RV4 at stop: %s", values(ins))
		start := time.Now()
		ins[coord].Stop()
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("Stop took %v while the canary was measured; want it at once", took)
		}
		for _, in := range ins {
			vis.StallCalls(in, 0)
		}
		time.Sleep(11 * time.Minute)
	})
}
