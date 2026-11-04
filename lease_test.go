package vis_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// Lease timing runs under synctest, so a 10-minute lease takes no real time.

func TestLeaseExpiryFallsBackToTheLayerBelow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 96, For: time.Hour, By: claude})
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})

		time.Sleep(10*time.Minute - time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("before the rule's lease ends: %d, want 48", got)
		}

		time.Sleep(time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 96 {
			t.Fatalf("after the rule's lease ends: %d, want the agent's 96", got)
		}

		time.Sleep(50 * time.Minute)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("after the agent's lease ends: %d, want the default 64", got)
		}
	})
}

func TestAnExpiringAgentChangeNeverErasesAPin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam})
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Minute, By: claude})
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := cacheSize.Get(in.Snapshot()); got != 3000 {
			t.Fatalf("cache size = %d, want the pin's 3000", got)
		}
		st, _ := in.Stack("test_cache_size")
		for _, e := range st {
			if e.Layer == vis.AgentLayer {
				t.Fatalf("expired agent entry still in the stack: %+v", e)
			}
		}
	})
}

func TestAPinWithAnExpiryEndsAndOneWithoutStays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, For: 30 * time.Minute, By: sam})
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 100, By: sam})
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		cfg := in.Snapshot()
		if got := cacheSize.Get(cfg); got != 1000 {
			t.Fatalf("expiring pin: %d, want back to 1000", got)
		}
		if got := batchSize.Get(cfg); got != 100 {
			t.Fatalf("pin without expiry: %d, want 100 still", got)
		}
	})
}

func TestReplacingALeaseRestartsItsClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: 10 * time.Minute, By: claude})
		time.Sleep(8 * time.Minute)
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2500, For: 10 * time.Minute, By: claude})
		time.Sleep(5 * time.Minute) // the first lease would have ended 3 minutes ago
		synctest.Wait()
		if got := cacheSize.Get(in.Snapshot()); got != 2500 {
			t.Fatalf("cache size = %d, want 2500 until the new lease ends", got)
		}
		time.Sleep(5 * time.Minute) // 18 minutes in: the new lease ends
		synctest.Wait()
		if got := cacheSize.Get(in.Snapshot()); got != 1000 {
			t.Fatalf("cache size = %d, want 1000 once the new lease ends", got)
		}
	})
}

// Each layer says what the knob falls back to when it ends.
func TestTheStackSaysWhatComesNext(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: claude})
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, For: time.Hour, By: sam})
	st, _ := in.Stack("test_cache_size")
	if st[0].Then != 2000 || st[1].Then != 1000 {
		t.Fatalf("stack = %+v, want the pin to fall back to 2000 and the agent's value to 1000", st)
	}
}

// A deliberate change that is about to run out says so, ahead of time.
func TestALeaseWarnsBeforeItEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		rec, err := in.Apply(vis.Tx{By: claude, For: 30 * time.Minute, Reason: "cpu", Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}}})
		if err != nil {
			t.Fatal(err)
		}
		ending := func() []vis.Event {
			var out []vis.Event
			for _, e := range in.History(50) {
				if e.Kind == "lease" && e.Fields["event"] == "ending" {
					out = append(out, e)
				}
			}
			return out
		}
		time.Sleep(27*time.Minute - time.Second) // 10% of 30 minutes is 3
		synctest.Wait()
		if len(ending()) != 0 {
			t.Fatal("warned too early")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		e := ending()
		if len(e) != 1 {
			t.Fatalf("got %d ending events, want 1", len(e))
		}
		f := e[0].Fields
		if f["lease_id"] != rec.Lease || f["knob"] != "test_cache_size" || f["value"] != int64(2000) || f["then"] != int64(1000) || f["left"] != "3m0s" || f["actor"] != "agent:claude" {
			t.Fatalf("ending event = %+v", f)
		}
	})
}

func TestRuleLeasesDoNotWarn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: time.Minute, By: sched})
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		for _, e := range in.History(50) {
			if e.Kind == "lease" {
				t.Fatalf("a rule's lease warned: %+v", e)
			}
		}
	})
}

func TestARevertedLeaseDoesNotWarn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		rec, _ := in.Apply(vis.Tx{By: claude, For: 30 * time.Minute, Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}}})
		in.Revert(rec.Lease, claude, "")
		time.Sleep(time.Hour)
		synctest.Wait()
		for _, e := range in.History(50) {
			if e.Kind == "lease" {
				t.Fatalf("a reverted lease warned: %+v", e)
			}
		}
	})
}

func TestAFreezeDelaysTheWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		in.Apply(vis.Tx{By: claude, For: 30 * time.Minute, Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}}})
		mustFreeze(t, in, sam, "")
		time.Sleep(time.Hour)
		synctest.Wait()
		for _, e := range in.History(50) {
			if e.Kind == "lease" {
				t.Fatalf("warned while the lease clock was stopped: %+v", e)
			}
		}
	})
}
