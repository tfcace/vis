package vis_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// A special value is one a setting gives its own meaning, outside its range:
// imageproxy's -timeout 0 is "no limit". vis never calculates with it, so it
// never has to know which side of the range "no limit" is on.
var (
	noLimit = vis.NewDuration("test_no_limit", vis.Range(time.Second, 5*time.Minute), vis.Default(time.Duration(0)),
		vis.Special(time.Duration(0), "no limit"), vis.Pull(vis.Agent, vis.Rule))
	limited = vis.NewDuration("test_limited", vis.Range(time.Second, 5*time.Minute), vis.Default(3*time.Second),
		vis.Special(time.Duration(0), "no limit"), vis.Pull(vis.Agent, vis.Rule))
	unbounded = vis.NewInt("test_unbounded", vis.Range(1, 100), vis.Default(10), vis.Special(0, "unlimited"), vis.Pull(vis.Agent))
)

// The code reads the special value as it always did: 0.
func TestASpecialValueIsTheCodesOwnValue(t *testing.T) {
	in := vis.NewInstance()
	if got := noLimit.Get(in.Snapshot()); got != 0 {
		t.Fatalf("value = %v, want 0, the code's own no limit", got)
	}
	i := info(t, "test_no_limit")
	if i.Format(0) != "no limit" || i.Format(int64(3*time.Second)) != "3s" {
		t.Fatalf("format: %q and %q, want no limit and 3s", i.Format(0), i.Format(int64(3*time.Second)))
	}
	if !i.HasSpecial || i.Special != 0 || i.SpecialName != "no limit" {
		t.Fatalf("info = %+v, want the special value 0 named no limit", i)
	}
}

// People and agents choose it by name, as they read it.
func TestPeopleAndAgentsChooseTheSpecialValueByName(t *testing.T) {
	in := vis.NewInstance()
	i := info(t, "test_limited")
	v, err := i.Parse("no limit")
	if err != nil || v != 0 {
		t.Fatalf("Parse(no limit) = %d, %v; want 0", v, err)
	}
	if _, err := in.Set(vis.Change{Knob: "test_limited", Value: 0, For: time.Hour, By: claude}); err != nil {
		t.Fatalf("an agent setting no limit: %v", err)
	}
	if _, err := in.Set(vis.Change{Knob: "test_limited", Value: 0, By: sam}); err != nil {
		t.Fatalf("a person setting no limit: %v", err)
	}
	if _, err := in.Set(vis.Change{Knob: "test_limited", Value: int64(500 * time.Millisecond), By: sam}); !errors.Is(err, vis.ErrOutOfRange) {
		t.Fatalf("500ms, outside the range and not special: %v, want out of range", err)
	} else if !strings.Contains(err.Error(), `or "no limit"`) {
		t.Fatalf("the error should name the special value too: %v", err)
	}
}

// A rule never moves a knob into or out of its special value: stepping from
// it means nothing, and stepping into it could turn a limit off.
func TestRulesNeverReachTheSpecialValue(t *testing.T) {
	in := vis.NewInstance()
	if _, err := in.Set(vis.Change{Knob: "test_limited", Value: 0, For: time.Hour, By: sched}); err == nil {
		t.Fatal("a rule set no limit; only people and agents may choose it")
	}
	if _, err := in.Apply(vis.Tx{By: sched, For: time.Hour, Reason: "r", Step: []vis.Step{{Knob: "test_limited", By: int64(-10 * time.Second)}}}); err != nil {
		t.Fatal(err)
	}
	if got := limited.Get(in.Snapshot()); got != time.Second {
		t.Fatalf("a step down past the floor: %v, want 1s, the range's floor, never no limit", got)
	}
	_, err := in.Apply(vis.Tx{By: sched, For: time.Hour, Reason: "r", Step: []vis.Step{{Knob: "test_no_limit", By: int64(-time.Second)}}})
	if err == nil || !strings.Contains(err.Error(), "no limit") {
		t.Fatalf("a step from no limit: %v, want an error naming no limit", err)
	}
}

func timeoutRule(k vis.Duration, s *signal) vis.RuleSpec {
	return vis.RuleSpec{
		Name: "fail_fast_" + k.Name(), Knob: k,
		Watch: vis.Measure{Name: "sched.p99", Unit: "ms", Read: s.read},
		Over:  vis.Fixed(5 * time.Millisecond),
		For:   3 * time.Second, Step: int64(-time.Second), Cooldown: 10 * time.Second, Lease: time.Hour, Deadband: vis.Fixed(time.Millisecond),
	}
}

// A rule on a knob at its special value stands aside, and says why.
func TestARuleStandsAsideAtTheSpecialValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(20)
		runRule(t, in, timeoutRule(noLimit, s))
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if got := noLimit.Get(in.Snapshot()); got != 0 {
			t.Fatalf("value = %v, want no limit, untouched", got)
		}
		st := stateOf(in, "fail_fast_test_no_limit")
		if st.State != "aside" || !strings.Contains(st.Aside, "no limit") || st.Moves != "" {
			t.Fatalf("state = %+v, want aside, saying the knob is no limit, with nowhere to move", st)
		}
	})
}

// From a value in the range, a rule steps down to the floor and no further.
func TestARuleStepsToTheFloorNotTheSpecialValue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(20)
		r := timeoutRule(limited, s)
		r.Stall = 100
		runRule(t, in, r)
		for v := int64(19); v > 10; v-- { // improving each time, never under the limit
			time.Sleep(10 * time.Second)
			s.set(v)
		}
		synctest.Wait()
		if got := limited.Get(in.Snapshot()); got != time.Second {
			t.Fatalf("value = %v, want 1s, the floor", got)
		}
		if st := stateOf(in, "fail_fast_test_limited"); st.State != "at_limit" || st.Moves != "1s..3s" {
			t.Fatalf("state = %+v, want at_limit within 1s..3s", st)
		}
	})
}

func TestADeclaredSpecialValueMustBeOutsideTheRange(t *testing.T) {
	mustPanic(t, "special inside the range", func() {
		vis.NewDuration("test_special_inside", vis.Range(0, time.Minute), vis.Default(time.Second), vis.Special(time.Duration(0), "no limit"))
	})
	mustPanic(t, "special without a name", func() {
		vis.NewDuration("test_special_unnamed", vis.Range(time.Second, time.Minute), vis.Default(time.Second), vis.Special(time.Duration(0), ""))
	})
	mustPanic(t, "special of the wrong type", func() {
		vis.NewDuration("test_special_int", vis.Range(time.Second, time.Minute), vis.Default(time.Second), vis.Special(0, "no limit"))
	})
	mustPanic(t, "a name that reads as a value", func() {
		vis.NewInt("test_special_numeric", vis.Range(1, 9), vis.Default(1), vis.Special(0, "0"))
	})
}

// MCP shows the special value by name, and takes it by name.
func TestMCPShowsAndTakesTheSpecialValueByName(t *testing.T) {
	in := vis.NewInstance()
	cs := connect(t, in, "oncall-secret", tokens)
	out, isErr := call(t, cs, "knobs", map[string]any{"knob": "test_unbounded"})
	if isErr {
		t.Fatal(out)
	}
	k := out["knobs"].([]any)[0].(map[string]any)
	if k["special"] != "unlimited" {
		t.Fatalf("knob = %v, want special: unlimited", k)
	}
	if out, isErr := call(t, cs, "set_knob", map[string]any{"set": map[string]any{"test_unbounded": "unlimited"}, "for": "10m", "reason": "r"}); isErr {
		t.Fatal(out)
	}
	if got := unbounded.Get(in.Snapshot()); got != 0 {
		t.Fatalf("value = %d, want 0, unlimited", got)
	}
	out, _ = call(t, cs, "knobs", map[string]any{"knob": "test_unbounded"})
	if k := out["knobs"].([]any)[0].(map[string]any); k["value"] != "unlimited" {
		t.Fatalf("value shown = %v, want unlimited", k["value"])
	}
}

// A flag or the environment may start the knob at its special value: -timeout 0
// keeps meaning no limit.
func TestAFlagOrTheEnvironmentMayChooseTheSpecialValue(t *testing.T) {
	if os.Getenv("VIS_TEST_FRESH") != "1" { // SetDefault refuses after vis.Start, which another test calls: run in a fresh process
		cmd := exec.Command(os.Args[0], "-test.run=^TestAFlagOrTheEnvironmentMayChooseTheSpecialValue$", "-test.count=1")
		cmd.Env = append(os.Environ(), "VIS_TEST_FRESH=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}
	if err := limited.SetDefault(0); err != nil || limited.Now() != 0 {
		t.Fatalf("SetDefault(0) = %v, now %v; want no limit", err, limited.Now())
	}
	if err := limited.SetDefault(500 * time.Millisecond); err == nil || !strings.Contains(err.Error(), `or "no limit"`) {
		t.Fatalf("SetDefault(500ms) = %v, want an error naming the range and no limit", err)
	}
	t.Setenv("VIS_T_NOLIMIT", "0s")
	k := vis.NewDuration("test_env_no_limit", vis.Range(time.Second, time.Minute), vis.Default(3*time.Second), vis.Special(time.Duration(0), "no limit"), vis.Env("VIS_T_NOLIMIT"))
	if got := k.Get(vis.NewInstance().Snapshot()); got != 0 {
		t.Fatalf("from VIS_T_NOLIMIT=0s: %v, want no limit", got)
	}
}
