package vis_test

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/tfcace/vis"
)

func TestFreezeRejectsAutomatedWritesButNotHumans(t *testing.T) {
	in := vis.NewInstance()
	mustFreeze(t, in, sam, "investigating the spike")
	for _, w := range []vis.Writer{rollout, claude, sched} {
		_, err := in.Set(vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: w})
		if !errors.Is(err, vis.ErrFrozen) {
			t.Fatalf("%v write while frozen: err = %v, want ErrFrozen", w, err)
		}
	}
	if eff := mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam}); eff.Value != 3000 {
		t.Fatalf("pin while frozen: %+v, want 3000", eff)
	}
}

func TestFreezeHoldsValuesInsteadOfMaskingLayers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
		mustFreeze(t, in, sam, "")
		time.Sleep(time.Hour) // well past the rule's lease
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("batch size = %d, want the rule's 48 held, not reverted or expired", got)
		}
	})
}

func TestAHumanCanStillResetAKnobWhileFrozen(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
	mustFreeze(t, in, sam, "")
	eff, err := in.Clear("test_batch_size", vis.RuleLayer, sam, "")
	if err != nil || eff.Value != 64 {
		t.Fatalf("clear while frozen: %+v, %v; want back to 64", eff, err)
	}
}

// The bypasses both reviews found: clearing a layer, or claiming the pin layer, during a freeze.
func TestOnlyPeopleChangeAnythingWhileFrozen(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 96, For: time.Hour, By: claude})
	mustFreeze(t, in, sam, "")
	if _, err := in.Clear("test_batch_size", vis.RuleLayer, claude, ""); !errors.Is(err, vis.ErrNotAllowed) {
		t.Fatalf("agent clearing the rule's mitigation: err = %v, want ErrNotAllowed", err)
	}
	if _, err := in.Clear("test_batch_size", vis.RuleLayer, sched, ""); !errors.Is(err, vis.ErrFrozen) {
		t.Fatalf("rule clearing its own layer during a freeze: err = %v, want ErrFrozen", err)
	}
	if _, err := in.Clear("test_batch_size", vis.AgentLayer, claude, ""); !errors.Is(err, vis.ErrFrozen) {
		t.Fatalf("agent clearing its own layer during a freeze: err = %v, want ErrFrozen", err)
	}
	if got := batchSize.Get(in.Snapshot()); got != 48 {
		t.Fatalf("batch size = %d, want the rule's 48 held", got)
	}
}

func TestAnyWriterMayFreezeButOnlyAPersonUnfreezes(t *testing.T) {
	in := vis.NewInstance()
	mustFreeze(t, in, claude, "p99 regression after my change")
	if err := in.Unfreeze(claude, ""); !errors.Is(err, vis.ErrNotAllowed) {
		t.Fatalf("agent unfreezing: err = %v, want ErrNotAllowed", err)
	}
	if err := in.Unfreeze(sam, ""); err != nil || in.FreezeState().Frozen {
		t.Fatalf("person unfreezing: %v, frozen=%v", err, in.FreezeState().Frozen)
	}
}

func TestLeaseClocksStopDuringAFreezeAndResumeAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, For: 10 * time.Minute, By: sam})
		time.Sleep(4 * time.Minute)
		mustFreeze(t, in, sam, "")

		time.Sleep(time.Hour)
		synctest.Wait()
		cfg := in.Snapshot()
		if batchSize.Get(cfg) != 48 || cacheSize.Get(cfg) != 3000 {
			t.Fatalf("after an hour frozen: batch %d, cache %d; want both held", batchSize.Get(cfg), cacheSize.Get(cfg))
		}
		if st, _ := in.Stack("test_batch_size"); st[0].Left != 6*time.Minute {
			t.Fatalf("rule lease left while frozen = %v, want 6m", st[0].Left)
		}

		if err := in.Unfreeze(sam, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(6*time.Minute - time.Second)
		synctest.Wait()
		if got := batchSize.Get(in.Snapshot()); got != 48 {
			t.Fatalf("just before the resumed lease ends: %d, want 48", got)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		cfg = in.Snapshot()
		if batchSize.Get(cfg) != 64 || cacheSize.Get(cfg) != 1000 {
			t.Fatalf("after the resumed leases end: batch %d, cache %d; want defaults", batchSize.Get(cfg), cacheSize.Get(cfg))
		}
	})
}

func TestFreezeSaysWhoWhenAndWhy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		if f := in.FreezeState(); f.Frozen {
			t.Fatalf("new instance is frozen: %+v", f)
		}
		start := time.Now()
		mustFreeze(t, in, sam, "investigating the spike")
		f := in.FreezeState()
		if !f.Frozen || f.By != sam || f.Reason != "investigating the spike" || !f.Since.Equal(start) || f.Epoch != 1 {
			t.Fatalf("freeze state = %+v", f)
		}
		mustFreeze(t, in, alex, "me too") // already frozen: the first freeze stands, the second is recorded, and the epoch rises
		if f := in.FreezeState(); f.By != sam || f.Epoch != 2 {
			t.Fatalf("second freeze: %+v, want sam's standing at epoch 2", f)
		}
		if err := in.Unfreeze(alex, "spike over"); err != nil {
			t.Fatal(err)
		}
		if f := in.FreezeState(); f.Frozen || f.By != alex || f.Reason != "spike over" || f.Epoch != 3 {
			t.Fatalf("after unfreeze: %+v, want unfrozen by alex at epoch 3", f)
		}
	})
}

func TestFreezeNeedsAnActorAndCapsTheReason(t *testing.T) {
	in := vis.NewInstance()
	if err := in.Freeze(vis.Writer{}, "x"); !errors.Is(err, vis.ErrActorRequired) {
		t.Fatalf("err = %v, want ErrActorRequired", err)
	}
	long := "a" + strings.Repeat("é", 100) // 201 bytes; byte 140 falls inside an é
	mustFreeze(t, in, sam, long)
	r := in.FreezeState().Reason
	if len(r) != 139 || !utf8.ValidString(r) || !strings.HasPrefix(long, r) {
		t.Fatalf("reason = %d bytes, valid=%v; want 139 bytes cut on a rune boundary", len(r), utf8.ValidString(r))
	}
	if err := in.Unfreeze(sam, ""); err != nil {
		t.Fatal(err)
	}
	junk := "\xff" + strings.Repeat("a", 200) // starts with an invalid byte: keep what's there, don't drop it all
	mustFreeze(t, in, sam, junk)
	if r := in.FreezeState().Reason; len(r) != 140 || !strings.HasPrefix(junk, r) {
		t.Fatalf("reason = %q (%d bytes), want the first 140 bytes", r, len(r))
	}
}

func mustFreeze(t *testing.T, in *vis.Instance, by vis.Writer, reason string) {
	t.Helper()
	if err := in.Freeze(by, reason); err != nil {
		t.Fatal(err)
	}
}
