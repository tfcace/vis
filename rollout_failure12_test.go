package vis_test

// These tests came from a twelfth review of the fleet layer, each a reproducer of a defect it found.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// R1: a finished rollout, then a rollout refused for its arguments: what
// does fleet's last_rollout show?
func TestARefusedRolloutKeepsTheLastOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2)
		defer stop()
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fix"})
		t.Logf("rollout 1: isErr=%v outcome=%v", isErr, out["outcome"])
		time.Sleep(time.Until(mustTime(t, out["measured_at"])) + 3*time.Second)
		f, _ := call(t, cs, "fleet", nil)
		lr, _ := f["last_rollout"].(map[string]any)
		t.Logf("before: state=%v undo=%v", lr["state"], lr["undo"])
		bb, _ := json.Marshal(lr)
		t.Logf("measured report, 3 members: %d bytes: %s", len(bb), bb)
		// a typo'd retry: for too short
		out2, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 3}, "for": "10s", "reason": "again"})
		t.Logf("rollout 2: isErr=%v err=%v", isErr, out2["error"])
		time.Sleep(3 * time.Second)
		for _, name := range []string{"a", "b"} {
			c := connectOver(t, n, ins[name], name+":707"+map[string]string{"a": "1", "b": "2"}[name], "oncall-secret")
			f, _ = call(t, c, "fleet", nil)
			lr, _ = f["last_rollout"].(map[string]any)
			t.Logf("after, via %s: outcome=%v state=%v why=%v undo=%v", name, lr["outcome"], lr["state"], lr["why"], lr["undo"])
			if lr["undo"] == nil || !strings.Contains(fmt.Sprint(lr["undo"]), "revert") {
				t.Errorf("via %s: a refused rollout replaced the finished one's report, undo line and all: %v", name, lr)
			}
		}
		b, _ := json.Marshal(lr)
		t.Logf("report bytes: %d", len(b))
	})
}

// R2: a member crashes and comes back at a new address under its name (a
// pod rescheduled, a container recreated) within seconds.
func TestARestartAtANewAddressIsntASharedName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		a, sa := peer(t, n, "cacheproxy", "a", "k", "", slo)
		b, sb := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, sc := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		_ = c
		defer func() { sa(); sc(); synctest.Wait() }()
		time.Sleep(20 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		vis.SkipLeave(b)
		sb()
		time.Sleep(time.Second)
		b2, sb2 := peerAt(t, n, "b", "b-new:7946", "a:7946", slo)
		_ = b2
		defer sb2()
		start := time.Now()
		var refused time.Duration
		for i := 0; i < 12; i++ {
			out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "b", "set": map[string]any{"fleet_test_knob": 2}, "for": "1m", "reason": "fix"})
			if isErr && strings.Contains(fmt.Sprint(out["error"]), "two instances answer as") {
				refused = time.Since(start)
				t.Logf("t+%v: refused: %v", time.Since(start).Round(time.Millisecond), out["error"])
				if i == 0 {
					t.Logf("warnings: %v", a.Warnings())
				}
			} else {
				t.Logf("t+%v: isErr=%v %v", time.Since(start).Round(time.Millisecond), isErr, out["error"])
			}
			time.Sleep(500 * time.Millisecond)
		}
		if refused > 0 {
			t.Errorf("a member restarted at a new address read as two instances sharing a name for %v: calls to it refused, told to fix VIS_INSTANCE_NAME", refused.Round(time.Second))
		}
	})
}

// R1b: as R1, the retry's value out of range: refused by every member, nothing changed.
func TestARolloutThatChangedNothingKeepsTheLastOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ins, n, stop := three(t, fast2)
		defer stop()
		time.Sleep(65 * time.Second)
		cs := connectOver(t, n, ins["a"], "a:7070", "oncall-secret")
		out, _ := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "fix"})
		time.Sleep(time.Until(mustTime(t, out["measured_at"])) + 3*time.Second)
		out2, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 99}, "for": "10m", "reason": "again"})
		t.Logf("rollout 2: isErr=%v outcome=%v why=%v", isErr, out2["outcome"], out2["why"])
		time.Sleep(3 * time.Second)
		f, _ := call(t, cs, "fleet", nil)
		lr, _ := f["last_rollout"].(map[string]any)
		t.Logf("after: outcome=%v state=%v undo=%v; values %s", lr["outcome"], lr["state"], lr["undo"], values(ins))
		if !strings.Contains(fmt.Sprint(lr["undo"]), "revert") {
			t.Errorf("a rollout that changed nothing replaced the finished one's report: %v", lr)
		}
	})
}
