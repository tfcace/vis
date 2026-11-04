package vis_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// Knob declarations are package-level in real code; tests declare their own with unique names.
var (
	cacheSize = vis.NewInt("test_cache_size", vis.Range(1, 10000), vis.Default(1000), vis.Unit("MiB"), vis.Pull(vis.Agent, vis.Rule, vis.Fleet))
	batchSize = vis.NewInt("test_batch_size", vis.Range(1, 512), vis.Default(64), vis.Pull(vis.Agent, vis.Rule))
	timeout   = vis.NewDuration("test_timeout", vis.Range(100*time.Millisecond, 10*time.Second), vis.Default(2*time.Second))
	_         = vis.NewInt("test_humans_only", vis.Range(0, 3), vis.Default(1)) // no Pull: only people may change it
)

func TestSnapshotStartsAtDefaults(t *testing.T) {
	in := vis.NewInstance()
	cfg := in.Snapshot()
	if got := cacheSize.Get(cfg); got != 1000 {
		t.Fatalf("cache size = %d, want 1000", got)
	}
	if got := timeout.Get(cfg); got != 2*time.Second {
		t.Fatalf("timeout = %v, want 2s", got)
	}
}

func TestSetChangesEffectiveValueButNotOldSnapshots(t *testing.T) {
	in := vis.NewInstance()
	before := in.Snapshot()
	eff, err := in.Set(vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: claude})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Value != 2000 || eff.Layer != vis.AgentLayer {
		t.Fatalf("effect = %+v, want 2000 from agent", eff)
	}
	if got := cacheSize.Get(in.Snapshot()); got != 2000 {
		t.Fatalf("new snapshot = %d, want 2000", got)
	}
	if got := cacheSize.Get(before); got != 1000 {
		t.Fatalf("old snapshot changed to %d; snapshots must be immutable", got)
	}
}

func TestInstancesAreIndependent(t *testing.T) {
	a, b := vis.NewInstance(), vis.NewInstance()
	if _, err := a.Set(vis.Change{Knob: "test_cache_size", Value: 3000, By: sam}); err != nil {
		t.Fatal(err)
	}
	if got := cacheSize.Get(b.Snapshot()); got != 1000 {
		t.Fatalf("instance b = %d, want its own default 1000", got)
	}
}

// The highest unexpired layer wins: default < fleet < agent < rule < pin.
func TestHigherLayerWinsAndSetReportsWhatTookEffect(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
	eff, err := in.Set(vis.Change{Knob: "test_batch_size", Value: 96, For: time.Hour, By: claude})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Value != 48 || eff.Layer != vis.RuleLayer {
		t.Fatalf("effect = %+v, want the rule's 48 still in effect", eff)
	}
	eff = mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 89, For: 30 * time.Minute, By: sam})
	if eff.Value != 89 || eff.Layer != vis.PinLayer {
		t.Fatalf("effect = %+v, want the pin's 89", eff)
	}
}

func TestStackShowsEveryLayerTopFirst(t *testing.T) {
	synctest.Test(t, testStackShowsEveryLayerTopFirst)
}

func testStackShowsEveryLayerTopFirst(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 96, For: time.Hour, By: claude})
	st, err := in.Stack("test_batch_size")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		l vis.Layer
		v int64
	}{{vis.RuleLayer, 48}, {vis.AgentLayer, 96}, {vis.DefaultLayer, 64}}
	if len(st) != len(want) {
		t.Fatalf("stack = %+v, want %d entries", st, len(want))
	}
	for i, w := range want {
		if st[i].Layer != w.l || st[i].Value != w.v {
			t.Fatalf("stack[%d] = %+v, want %v=%d", i, st[i], w.l, w.v)
		}
	}
	if st[1].By != claude || st[1].Left != time.Hour {
		t.Fatalf("agent entry = %+v, want actor and 1h left", st[1])
	}
}

func TestSetRejectsInvalidChanges(t *testing.T) {
	in := vis.NewInstance()
	cases := []struct {
		name string
		c    vis.Change
		want error
	}{
		{"unknown knob", vis.Change{Knob: "nope", Value: 1, By: sam}, vis.ErrUnknownKnob},
		{"above range", vis.Change{Knob: "test_cache_size", Value: 10001, By: sam}, vis.ErrOutOfRange},
		{"below range", vis.Change{Knob: "test_cache_size", Value: 0, By: sam}, vis.ErrOutOfRange},
		{"agent needs a lease", vis.Change{Knob: "test_cache_size", Value: 5, By: claude}, vis.ErrLeaseRequired},
		{"rule needs a lease", vis.Change{Knob: "test_cache_size", Value: 5, By: sched}, vis.ErrLeaseRequired},
		{"fleet needs a lease", vis.Change{Knob: "test_cache_size", Value: 5, By: rollout}, vis.ErrLeaseRequired},
		{"writer required", vis.Change{Knob: "test_cache_size", Value: 5}, vis.ErrActorRequired},
		{"writer needs an id", vis.Change{Knob: "test_cache_size", Value: 5, By: vis.Writer{Kind: vis.Human}}, vis.ErrActorRequired},
		{"negative lease", vis.Change{Knob: "test_cache_size", Value: 5, For: -time.Minute, By: sam}, vis.ErrLeaseRequired},
		{"agent not in pull", vis.Change{Knob: "test_humans_only", Value: 2, For: time.Hour, By: claude}, vis.ErrNotAllowed},
		{"fleet not in pull", vis.Change{Knob: "test_batch_size", Value: 2, For: time.Hour, By: rollout}, vis.ErrNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := in.Set(tc.c); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if got := cacheSize.Get(in.Snapshot()); got != 1000 {
		t.Fatalf("rejected changes leaked: cache size = %d", got)
	}
}

func TestWritersLandInTheirOwnLayer(t *testing.T) {
	in := vis.NewInstance()
	for _, tc := range []struct {
		by   vis.Writer
		want vis.Layer
	}{{rollout, vis.FleetLayer}, {claude, vis.AgentLayer}, {sched, vis.RuleLayer}, {sam, vis.PinLayer}} {
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: tc.by})
		st, _ := in.Stack("test_cache_size")
		if st[0].Layer != tc.want || st[0].By != tc.by {
			t.Fatalf("%v wrote %+v, want the %v layer", tc.by, st[0], tc.want)
		}
	}
}

func TestPeopleMayChangeAnyKnob(t *testing.T) {
	in := vis.NewInstance()
	if eff := mustSet(t, in, vis.Change{Knob: "test_humans_only", Value: 3, By: sam}); eff.Value != 3 {
		t.Fatalf("effect = %+v, want 3", eff)
	}
}

func TestAutomatedWritersClearOnlyTheirOwnLayer(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 48, For: 10 * time.Minute, By: sched})
	if _, err := in.Clear("test_batch_size", vis.RuleLayer, claude, ""); !errors.Is(err, vis.ErrNotAllowed) {
		t.Fatalf("agent clearing the rule layer: err = %v, want ErrNotAllowed", err)
	}
	if eff, err := in.Clear("test_batch_size", vis.RuleLayer, sched, ""); err != nil || eff.Value != 64 {
		t.Fatalf("rule clearing its own layer: %+v, %v", eff, err)
	}
}

func TestClearRemovesOneLayer(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam})
	eff, err := in.Clear("test_cache_size", vis.PinLayer, sam, "")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Value != 1000 || eff.Layer != vis.DefaultLayer {
		t.Fatalf("effect = %+v, want back to default 1000", eff)
	}
}

var lateN atomic.Int64

// A package-level instance can start before another package's init declares its knobs.
func TestKnobsDeclaredAfterTheInstanceStartedWork(t *testing.T) {
	in := vis.NewInstance()
	name := fmt.Sprintf("test_late_%d", lateN.Add(1)) // unique across -count runs
	late := vis.NewInt(name, vis.Range(1, 10), vis.Default(5))
	var seen int
	late.BindIn(in, func(n int) { seen = n })
	if seen != 5 {
		t.Fatalf("Bind started from %d, want the default 5", seen)
	}
	mustSet(t, in, vis.Change{Knob: name, Value: 7, By: sam})
	if got := late.Get(in.Snapshot()); got != 7 || seen != 7 {
		t.Fatalf("late knob = %d, watcher saw %d; want 7 and 7", got, seen)
	}
}

func TestDeclarationsAreValidated(t *testing.T) {
	mustPanic(t, "default outside range", func() { vis.NewInt("bad_default", vis.Range(1, 10), vis.Default(11)) })
	mustPanic(t, "missing range", func() { vis.NewInt("no_range", vis.Default(1)) })
	mustPanic(t, "duplicate name", func() { vis.NewInt("test_cache_size", vis.Range(1, 10), vis.Default(1)) })
}

// Untyped numbers on a Duration knob would mean nanoseconds; that mistake fails at declaration.
func TestOptionsMustMatchTheKnobType(t *testing.T) {
	mustPanic(t, "int range on a duration", func() { vis.NewDuration("d_int_range", vis.Range(10, 5000), vis.Default(time.Duration(20))) })
	// numerically valid, so only the type mismatch can cause the panic
	mustPanic(t, "int default on a duration", func() { vis.NewDuration("d_int_def", vis.Range(time.Duration(1), time.Duration(10)), vis.Default(5)) })
	mustPanic(t, "duration range on an int", func() { vis.NewInt("i_dur_range", vis.Range(time.Duration(1), time.Duration(10)), vis.Default(5)) })
}

func TestDurationsReadAsDurationsInErrors(t *testing.T) {
	in := vis.NewInstance()
	_, err := in.Set(vis.Change{Knob: "test_timeout", Value: int64(time.Minute), By: sam})
	if !errors.Is(err, vis.ErrOutOfRange) || !strings.Contains(err.Error(), "test_timeout=1m0s, allowed 100ms..10s") {
		t.Fatalf("err = %v, want the range in durations", err)
	}
}

func mustSet(t *testing.T, in *vis.Instance, c vis.Change) vis.Effect {
	t.Helper()
	eff, err := in.Set(c)
	if err != nil {
		t.Fatalf("set %+v: %v", c, err)
	}
	return eff
}

func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: want panic", name)
		}
	}()
	f()
}

// A flag can supply a knob's default: set it at startup. A flag is user
// input, so a value outside the range is an error to report, not a panic, and
// the range is there to say in the message.
func TestAKnobsDefaultCanComeFromAFlag(t *testing.T) {
	if os.Getenv("VIS_TEST_FRESH") != "1" { // SetDefault refuses after vis.Start, which another test calls: run in a fresh process
		cmd := exec.Command(os.Args[0], "-test.run=^TestAKnobsDefaultCanComeFromAFlag$", "-test.count=1")
		cmd.Env = append(os.Environ(), "VIS_TEST_FRESH=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	var bound []int
	early := vis.NewInt(uniq("test_flagged_bound"), vis.Range(1, 100), vis.Default(10))
	early.Bind(func(n int) { bound = append(bound, n) }) // Bind before SetDefault: the default instance exists already
	if err := early.SetDefault(30); err != nil || early.Now() != 30 || !slices.Equal(bound, []int{10, 30}) {
		t.Fatalf("after SetDefault: %v, now %d, bound saw %v; want 30, seen by Bind", err, early.Now(), bound)
	}
	k := vis.NewInt(uniq("test_flagged"), vis.Range(1, 100), vis.Default(10))
	if err := k.SetDefault(40); err != nil {
		t.Fatal(err)
	}
	if got := k.Get(vis.NewInstance().Snapshot()); got != 40 {
		t.Fatalf("a new instance = %d, want 40", got)
	}
	err := k.SetDefault(1000)
	if err == nil || !strings.Contains(err.Error(), "1..100") {
		t.Fatalf("out of range = %v, want an error naming the range", err)
	}
	if lo, hi := k.Range(); lo != 1 || hi != 100 {
		t.Fatalf("Range = %d..%d", lo, hi)
	}
	d := vis.NewDuration(uniq("test_flagged_d"), vis.Range(time.Millisecond, time.Second), vis.Default(100*time.Millisecond))
	if lo, hi := d.Range(); lo != time.Millisecond || hi != time.Second {
		t.Fatalf("Duration Range = %v..%v", lo, hi)
	}
}

// A computed default, such as one per CPU, is the code's guess; the
// environment is what the operator set for this process, so it wins. A bad
// value in it still falls back, now to the computed default.
func TestTheEnvironmentBeatsAComputedDefault(t *testing.T) {
	if os.Getenv("VIS_TEST_FRESH") != "1" { // SetDefault refuses after vis.Start: run in a fresh process
		cmd := exec.Command(os.Args[0], "-test.run=^TestTheEnvironmentBeatsAComputedDefault$", "-test.count=1")
		cmd.Env = append(os.Environ(), "VIS_TEST_FRESH=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	t.Setenv("VIS_T_AUTO", "2")
	t.Setenv("VIS_T_AUTO_BAD", "0")
	k := vis.NewInt("env_t_auto", vis.Range(1, 64), vis.Default(1), vis.Env("VIS_T_AUTO"))
	b := vis.NewInt("env_t_auto_bad", vis.Range(1, 64), vis.Default(1), vis.Env("VIS_T_AUTO_BAD"))
	if err := k.SetDefault(3); err != nil || k.Now() != 2 {
		t.Fatalf("SetDefault(3) = %v, now %d; want 2, from VIS_T_AUTO", err, k.Now())
	}
	if i := info(t, "env_t_auto"); i.Default != 2 || !i.FromEnv {
		t.Fatalf("info = %+v, want default 2 from VIS_T_AUTO", i)
	}
	if err := k.SetDefault(100); err == nil {
		t.Fatal("SetDefault(100) = nil, want the range error even when the environment wins")
	}
	if err := b.SetDefault(3); err != nil || b.Now() != 3 {
		t.Fatalf("SetDefault(3) = %v, now %d; want 3, since VIS_T_AUTO_BAD is out of range", err, b.Now())
	}
	want := `VIS_T_AUTO_BAD="0" is outside env_t_auto_bad's range 1..64, so env_t_auto_bad keeps its default 3`
	if all := strings.Join(vis.NewInstance().Warnings(), "\n"); !strings.Contains(all, want) {
		t.Fatalf("warnings lack %q:\n%s", want, all)
	}
}

// Bind builds state from a knob and keeps it in step: the current value
// before Bind returns, then every change in order, with nothing slipping in between.
func TestBindBuildsStateAndKeepsItInStep(t *testing.T) {
	in := vis.NewInstance()
	var mu sync.Mutex
	var seen []int
	batchSize.BindIn(in, func(n int) {
		mu.Lock()
		seen = append(seen, n)
		mu.Unlock()
	})
	mu.Lock()
	first := slices.Clone(seen)
	mu.Unlock()
	if !slices.Equal(first, []int{64}) {
		t.Fatalf("on Bind: %v, want the current value", first)
	}
	if _, err := in.Apply(vis.Tx{By: sam, Reason: "test", Set: []vis.Write{{Knob: batchSize.Name(), Value: 32}}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []int{64, 32}) {
		t.Fatalf("after a change: %v", seen)
	}
}

// A change racing Bind lands after the initial value, never before it.
func TestBindAgainstARacingChange(t *testing.T) {
	for range 200 {
		in := vis.NewInstance()
		go in.Apply(vis.Tx{By: sam, Reason: "race", Set: []vis.Write{{Knob: batchSize.Name(), Value: 32}}})
		var mu sync.Mutex
		last := -1
		batchSize.BindIn(in, func(n int) { mu.Lock(); last = n; mu.Unlock() })
		deadline := time.Now().Add(time.Second)
		for {
			mu.Lock()
			l := last
			mu.Unlock()
			if l == 32 || time.Now().After(deadline) {
				if l != 32 {
					t.Fatalf("state built from %d, want the latest value 32", l)
				}
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
}
