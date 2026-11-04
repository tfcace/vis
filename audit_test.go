package vis_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// auditLog captures the instance's audit records as decoded JSON lines.
type auditLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (a *auditLog) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.Write(p)
}

func (a *auditLog) records(t *testing.T) []map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(a.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func newAudited() (*vis.Instance, *auditLog) {
	a := &auditLog{}
	return vis.NewInstance(vis.WithLogger(slog.New(slog.NewJSONHandler(a, nil)))), a
}

func TestSetIsAuditedWithWhatTookEffect(t *testing.T) {
	in, a := newAudited()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched, Reason: "sched.p99 > 5ms"})
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 96, For: time.Hour, By: claude})
	rs := a.records(t)
	if len(rs) != 2 {
		t.Fatalf("got %d records, want 2: %v", len(rs), rs)
	}
	want := map[string]any{"msg": "vis.change", "op": "set", "knob": "test_batch_size", "layer": "rule", "actor": "rule:sched",
		"value": 48.0, "old": 64.0, "new": 48.0, "in_effect": "rule", "lease": "10m0s", "reason": "sched.p99 > 5ms"}
	assertFields(t, rs[0], want)
	// the agent's write lands under the rule, and the record says so
	assertFields(t, rs[1], map[string]any{"layer": "agent", "value": 96.0, "old": 48.0, "new": 48.0, "in_effect": "rule"})
}

func TestASecondFreezeIsRecordedButTheFirstStands(t *testing.T) {
	in, a := newAudited()
	mustFreeze(t, in, sam, "investigating")
	mustFreeze(t, in, alex, "me too")
	rs := a.records(t)
	if len(rs) != 2 {
		t.Fatalf("got %d records, want 2", len(rs))
	}
	assertFields(t, rs[1], map[string]any{"msg": "vis.freeze", "actor": "human:alex", "reason": "me too", "epoch": 2.0, "already_frozen_by": "human:sam"})
}

func TestClearCarriesAReason(t *testing.T) {
	in, a := newAudited()
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam})
	if _, err := in.Clear("test_cache_size", vis.PinLayer, sam, "done testing"); err != nil {
		t.Fatal(err)
	}
	assertFields(t, a.records(t)[1], map[string]any{"op": "clear", "reason": "done testing"})
}

func TestDurationKnobsAreAuditedAsDurations(t *testing.T) {
	in, a := newAudited()
	mustSet(t, in, vis.Change{Knob: "test_timeout", Value: int64(5 * time.Second), By: sam})
	assertFields(t, a.records(t)[0], map[string]any{"value": "5s", "old": "2s", "new": "5s"})
}

func TestClearExpiryAndFreezeAreAudited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in, a := newAudited()
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Minute, By: claude})
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 100, By: sam})
		if _, err := in.Clear("test_batch_size", vis.PinLayer, sam, ""); err != nil {
			t.Fatal(err)
		}
		mustFreeze(t, in, sam, "investigating")
		if err := in.Unfreeze(alex, "spike over"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		rs := a.records(t)
		if len(rs) != 7 {
			t.Fatalf("got %d records, want 7: %v", len(rs), rs)
		}
		assertFields(t, rs[2], map[string]any{"msg": "vis.change", "op": "clear", "knob": "test_batch_size", "layer": "pin", "actor": "human:sam", "old": 100.0, "new": 64.0})
		assertFields(t, rs[3], map[string]any{"msg": "vis.freeze", "actor": "human:sam", "reason": "investigating", "epoch": 1.0})
		assertFields(t, rs[4], map[string]any{"msg": "vis.unfreeze", "actor": "human:alex", "reason": "spike over", "epoch": 2.0})
		assertFields(t, rs[5], map[string]any{"msg": "vis.lease", "event": "ending", "knob": "test_cache_size", "left": "6s"}) // a tenth of the lease ahead
		assertFields(t, rs[6], map[string]any{"msg": "vis.change", "op": "expire", "knob": "test_cache_size", "layer": "agent", "actor": "lease", "old": 2000.0, "new": 1000.0})
	})
}

func TestBindSeesEffectiveChangesOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		var got []int
		batchSize.BindIn(in, func(n int) { got = append(got, n) })
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
		mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 96, For: time.Hour, By: claude}) // under the rule: no change
		time.Sleep(10 * time.Minute)                                                               // the rule's lease ends: 48 → 96
		synctest.Wait()
		if want := []int{64, 48, 96}; !slices.Equal(got, want) {
			t.Fatalf("Bind calls = %v, want %v: the value it starts from, then each effective change", got, want)
		}
	})
}

func TestBindMayChangeAnotherKnob(t *testing.T) {
	in := vis.NewInstance()
	cacheSize.BindIn(in, func(n int) {
		if n == 2000 {
			mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 32, By: sam})
		}
	})
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, By: sam})
	if got := batchSize.Get(in.Snapshot()); got != 32 {
		t.Fatalf("batch size = %d, want 32 set from inside Bind's callback", got)
	}
}

func TestConcurrentReadersAndWriters(t *testing.T) {
	in := vis.NewInstance()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 200 {
				if _, err := in.Set(vis.Change{Knob: "test_cache_size", Value: int64(1 + (w*200+i)%9999), By: sam}); err != nil {
					t.Error(err)
				}
			}
		})
		wg.Go(func() {
			for range 200 {
				if v := cacheSize.Get(in.Snapshot()); v < 1 || v > 10000 {
					t.Errorf("read %d outside the range", v)
				}
			}
		})
	}
	wg.Wait()
}

func assertFields(t *testing.T, rec map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("record %v: %s = %v, want %v", rec, k, rec[k], v)
		}
	}
}

// A panicking watcher is the app's bug; it must not silence every later notification.
func TestAPanickingWatcherDoesNotStopLaterNotifications(t *testing.T) {
	in, a := newAudited()
	calls := 0
	cacheSize.BindIn(in, func(int) {
		calls++
		if calls == 2 { // the first change, after the value Bind starts from
			panic("boom")
		}
	})
	for _, v := range []int64{2000, 3000, 4000} {
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: v, By: sam})
	}
	if calls != 4 {
		t.Fatalf("watcher called %d times, want 4: its first value, then three changes", calls)
	}
	var panics int
	for _, r := range a.records(t) {
		if r["msg"] == "vis.onchange_panic" {
			panics++
			assertFields(t, r, map[string]any{"knob": "test_cache_size", "panic": "boom"})
		}
	}
	if panics != 1 {
		t.Fatalf("got %d vis.onchange_panic records, want 1", panics)
	}
}

// Changes made from inside a watcher are delivered in the order they were made.
func TestWatchersSeeChangesInPublishOrder(t *testing.T) {
	in := vis.NewInstance()
	var got []int
	batchSize.BindIn(in, func(n int) { got = append(got, n) })
	cacheSize.BindIn(in, func(n int) {
		if n != 2000 {
			return
		}
		for _, v := range []int64{10, 20, 30} {
			mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: v, By: sam})
		}
	})
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, By: sam})
	if want := []int{64, 10, 20, 30}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
