package vis_test

import (
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

func TestApplySetsSeveralKnobsInOneStep(t *testing.T) {
	in := vis.NewInstance()
	rec, err := in.Apply(vis.Tx{By: claude, For: time.Hour, Reason: "hit rate 71%",
		Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}, {Knob: "test_batch_size", Value: 32}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := in.Snapshot()
	if cacheSize.Get(cfg) != 2000 || batchSize.Get(cfg) != 32 {
		t.Fatalf("snapshot: cache %d, batch %d; want 2000 and 32", cacheSize.Get(cfg), batchSize.Get(cfg))
	}
	if rec.Change == 0 || rec.Lease == 0 || len(rec.Effects) != 2 {
		t.Fatalf("record = %+v, want a change ID, a lease ID and two effects", rec)
	}
	if e := rec.Effects[1]; e.Knob != "test_batch_size" || e.Value != 32 || e.Layer != vis.AgentLayer {
		t.Fatalf("second effect = %+v", e)
	}
}

func TestATransactionIsAllOrNothing(t *testing.T) {
	in, a := newAudited()
	_, err := in.Apply(vis.Tx{By: claude, For: time.Hour,
		Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}, {Knob: "test_batch_size", Value: 9999}}})
	if !errors.Is(err, vis.ErrOutOfRange) {
		t.Fatalf("err = %v, want ErrOutOfRange", err)
	}
	if got := cacheSize.Get(in.Snapshot()); got != 1000 {
		t.Fatalf("cache size = %d; a failed transaction must change nothing", got)
	}
	if rs := a.records(t); len(rs) != 0 {
		t.Fatalf("a failed transaction was audited: %v", rs)
	}
}

func TestATransactionNamesEachKnobOnce(t *testing.T) {
	in := vis.NewInstance()
	_, err := in.Apply(vis.Tx{By: sam, Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}}, Clear: []vis.Removal{{Knob: "test_cache_size", Layer: vis.AgentLayer}}})
	if !errors.Is(err, vis.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

// Values set together expire together: no snapshot ever shows one without the other.
func TestValuesSetTogetherExpireTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, a := newAudited()
		var batchWhenCacheReverted = -1
		calls := 0
		cacheSize.BindIn(in, func(n int) {
			if calls++; calls > 1 && n == 1000 { // the revert, not the value Bind starts from
				batchWhenCacheReverted = batchSize.Get(in.Snapshot())
			}
		})
		if _, err := in.Apply(vis.Tx{By: claude, For: 10 * time.Minute,
			Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}, {Knob: "test_batch_size", Value: 32}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if batchWhenCacheReverted != 64 {
			t.Fatalf("when cache reverted, batch was %d; want 64, reverted in the same step", batchWhenCacheReverted)
		}
		var expiries []map[string]any
		for _, r := range a.records(t) {
			if r["op"] == "expire" {
				expiries = append(expiries, r)
			}
		}
		if len(expiries) != 2 || expiries[0]["change"] != expiries[1]["change"] {
			t.Fatalf("expiry records %v; want two that share one change ID", expiries)
		}
	})
}

func TestAReplacedValueLeavesItsLeaseGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		if _, err := in.Apply(vis.Tx{By: claude, For: 10 * time.Minute,
			Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}, {Knob: "test_batch_size", Value: 32}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Minute)
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 40, For: time.Hour, By: claude})
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		cfg := in.Snapshot()
		if cacheSize.Get(cfg) != 1000 || batchSize.Get(cfg) != 40 {
			t.Fatalf("cache %d, batch %d; want cache expired with its group, batch kept at its newer 40", cacheSize.Get(cfg), batchSize.Get(cfg))
		}
	})
}

func TestRevertEndsALeaseGroupEarly(t *testing.T) {
	in, a := newAudited()
	rec, err := in.Apply(vis.Tx{By: claude, For: time.Hour,
		Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}, {Knob: "test_batch_size", Value: 32}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Revert(rec.Lease, sam, "wrong call"); err != nil {
		t.Fatal(err)
	}
	cfg := in.Snapshot()
	if cacheSize.Get(cfg) != 1000 || batchSize.Get(cfg) != 64 {
		t.Fatalf("after revert: cache %d, batch %d; want defaults", cacheSize.Get(cfg), batchSize.Get(cfg))
	}
	rs := a.records(t)
	assertFields(t, rs[len(rs)-1], map[string]any{"op": "revert", "actor": "human:sam", "reason": "wrong call", "lease_id": float64(rec.Lease)})
	if _, err := in.Revert(rec.Lease, sam, ""); !errors.Is(err, vis.ErrUnknownLease) {
		t.Fatalf("reverting twice: err = %v, want ErrUnknownLease", err)
	}
}

func TestRevertIsGatedLikeAnyChange(t *testing.T) {
	in := vis.NewInstance()
	rec, err := in.Apply(vis.Tx{By: sched, For: time.Hour, Set: []vis.Write{{Knob: "test_batch_size", Value: 48}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Revert(rec.Lease, claude, ""); !errors.Is(err, vis.ErrNotAllowed) {
		t.Fatalf("agent reverting a rule's lease: err = %v, want ErrNotAllowed", err)
	}
	mustFreeze(t, in, sam, "")
	if _, err := in.Revert(rec.Lease, sched, ""); !errors.Is(err, vis.ErrFrozen) {
		t.Fatalf("rule reverting during a freeze: err = %v, want ErrFrozen", err)
	}
}

func TestOneChangeIDAcrossItsAuditRecords(t *testing.T) {
	in, a := newAudited()
	rec, err := in.Apply(vis.Tx{By: claude, For: time.Hour,
		Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}, {Knob: "test_batch_size", Value: 32}}})
	if err != nil {
		t.Fatal(err)
	}
	rs := a.records(t)
	if len(rs) != 2 {
		t.Fatalf("got %d records, want one per knob", len(rs))
	}
	for _, r := range rs {
		assertFields(t, r, map[string]any{"change": float64(rec.Change), "lease_id": float64(rec.Lease)})
	}
}

func TestSetReportsItsChangeAndLease(t *testing.T) {
	in := vis.NewInstance()
	eff := mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: claude})
	if eff.Change == 0 || eff.Lease == 0 {
		t.Fatalf("effect = %+v, want change and lease IDs", eff)
	}
	if eff := mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam}); eff.Lease != 0 {
		t.Fatalf("a pin without expiry has lease %d, want 0", eff.Lease)
	}
	if st, _ := in.Stack("test_cache_size"); st[1].Lease != eff.Lease || st[1].Change != eff.Change {
		t.Fatalf("stack entry %+v doesn't carry the agent's change and lease", st[1])
	}
}

func TestAStepMovesFromTheValueInEffect(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 96, For: time.Hour, By: claude})
	rec, err := in.Apply(vis.Tx{By: sched, For: 10 * time.Minute, Step: []vis.Step{{Knob: "test_batch_size", By: -16}}})
	if err != nil {
		t.Fatal(err)
	}
	if e := rec.Effects[0]; e.Value != 80 || e.Layer != vis.RuleLayer {
		t.Fatalf("effect = %+v, want 96-16 = 80 from the rule", e)
	}
}

func TestAStepIsClampedToTheRange(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 10, For: time.Hour, By: sched})
	rec, err := in.Apply(vis.Tx{By: sched, For: 10 * time.Minute, Step: []vis.Step{{Knob: "test_batch_size", By: -16}}})
	if err != nil || rec.Effects[0].Value != 1 {
		t.Fatalf("step below the floor: %+v, %v; want clamped to 1", rec, err)
	}
}

// At a bound a step can't move, so it writes nothing: a rule stuck at the floor
// can't keep renewing its own lease there.
func TestAStepThatCannotMoveWritesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, a := newAudited()
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 1, For: 10 * time.Minute, By: sched})
		time.Sleep(9 * time.Minute)
		rec, err := in.Apply(vis.Tx{By: sched, For: 10 * time.Minute, Step: []vis.Step{{Knob: "test_batch_size", By: -16}}})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Lease != 0 || rec.Effects[0].Value != 1 {
			t.Fatalf("record = %+v, want no new lease and the value still 1", rec)
		}
		if n := len(a.records(t)); n != 1 {
			t.Fatalf("got %d audit records, want only the original set", n)
		}
		time.Sleep(time.Minute) // the original lease still ends on time
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 64 {
			t.Fatalf("batch size = %d, want back to 64", got)
		}
	})
}

func TestExtremeStepsDoNotOverflow(t *testing.T) {
	in := vis.NewInstance()
	for _, by := range []int64{math.MinInt64, math.MaxInt64} {
		rec, err := in.Apply(vis.Tx{By: sam, Step: []vis.Step{{Knob: "test_batch_size", By: by}}})
		if err != nil {
			t.Fatal(err)
		}
		if v := rec.Effects[0].Value; v != 1 && v != 512 {
			t.Fatalf("step %d gave %d, want a bound", by, v)
		}
	}
}

// The UI's plan box: what a change would do, with every check Apply makes, and nothing changed.
func TestPreviewSaysWhatWouldTakeEffect(t *testing.T) {
	in, a := newAudited()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
	before := len(a.records(t))
	rec, err := in.Preview(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_batch_size", Value: 96}, {Knob: "test_cache_size", Value: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	if e := rec.Effects[0]; e.Value != 48 || e.Layer != vis.RuleLayer {
		t.Fatalf("batch preview = %+v, want the rule's 48 still in effect", e)
	}
	if e := rec.Effects[1]; e.Value != 2000 || e.Layer != vis.AgentLayer {
		t.Fatalf("cache preview = %+v, want 2000 from the agent", e)
	}
	if rec.Change != 0 || rec.Lease != 0 {
		t.Fatalf("preview = %+v, want no change or lease IDs", rec)
	}
	if cacheSize.Get(in.Snapshot()) != 1000 || len(a.records(t)) != before {
		t.Fatal("preview changed something")
	}
	if p, _ := in.Preview(vis.Tx{By: sam, For: 30 * time.Minute, Set: []vis.Write{{Knob: "test_batch_size", Value: 89}}}); p.Effects[0].Value != 89 || p.Effects[0].Layer != vis.PinLayer {
		t.Fatalf("pin preview = %+v, want the pin over the rule", p.Effects[0])
	}
}

func TestPreviewFailsWhereApplyWould(t *testing.T) {
	in := vis.NewInstance()
	mustFreeze(t, in, sam, "")
	if _, err := in.Preview(vis.Tx{By: claude, For: time.Hour, Set: []vis.Write{{Knob: "test_cache_size", Value: 2000}}}); !errors.Is(err, vis.ErrFrozen) {
		t.Fatalf("err = %v, want ErrFrozen", err)
	}
	if _, err := in.Preview(vis.Tx{By: sam, Set: []vis.Write{{Knob: "test_cache_size", Value: 0}}}); !errors.Is(err, vis.ErrOutOfRange) {
		t.Fatalf("err = %v, want ErrOutOfRange", err)
	}
}
