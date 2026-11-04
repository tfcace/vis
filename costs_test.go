package vis_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/tfcace/vis"
)

func TestKnobsSayWhatTheyUse(t *testing.T) {
	name := fmt.Sprintf("test_spender_%d", lateN.Add(1))
	vis.NewInt(name, vis.Range(1, 9), vis.Default(6), vis.Uses(vis.CPU, vis.Memory))
	k, _ := vis.Lookup(name)
	if !slices.Equal(k.Uses, []vis.Resource{vis.CPU, vis.Memory}) {
		t.Fatalf("uses = %v", k.Uses)
	}
}

// Levers for a resource: the knobs that spend it, the most expensive setting first.
func TestLeversRankByHowExpensiveTheSettingIs(t *testing.T) {
	n := lateN.Add(1)
	hot := vis.NewInt(fmt.Sprintf("test_hot_%d", n), vis.Range(1, 9), vis.Default(9), vis.Uses(vis.CPU), vis.About("costly"))
	cool := vis.NewInt(fmt.Sprintf("test_cool_%d", n), vis.Range(0, 100), vis.Default(20), vis.Uses(vis.CPU))
	quiet := vis.NewInt(fmt.Sprintf("test_quiet_%d", n), vis.Range(-4, 8), vis.Default(-4), vis.Saves(vis.CPU)) // debug logging: lower spends more
	in := vis.NewInstance()
	var got []string
	for _, l := range in.Levers(vis.CPU) {
		if l.Knob == hot.Name() || l.Knob == cool.Name() || l.Knob == quiet.Name() {
			got = append(got, fmt.Sprintf("%s@%.2f", l.Knob, l.Position))
		}
	}
	want := []string{hot.Name() + "@1.00", quiet.Name() + "@1.00", cool.Name() + "@0.20"}
	if !slices.Equal(got, want) {
		t.Fatalf("levers = %v, want %v", got, want)
	}
	for _, l := range in.Levers(vis.CPU) {
		if l.Knob == hot.Name() && (l.About != "costly" || l.Value != "9") {
			t.Fatalf("a lever should carry its value and what it costs: %+v", l)
		}
	}
	if _, err := in.Set(vis.Change{Knob: hot.Name(), Value: 1, By: sam}); err != nil {
		t.Fatal(err)
	}
	for _, l := range in.Levers(vis.CPU) {
		if l.Knob == hot.Name() && l.Position != 0 {
			t.Fatalf("after turning it down: %+v, want position 0", l)
		}
	}
}

// A knob's costs say what turning it up uses and what it saves: CPU and
// memory, which vis measures, or one of the app's own signals.
func TestCostsSayWhatTurningAKnobUpUsesAndSaves(t *testing.T) {
	fetches := vis.NewGauge(uniq("test_origin_fetches"))
	k := vis.NewInt(uniq("test_cache"), vis.Range(1, 100), vis.Default(10), vis.Uses(vis.Memory), vis.Saves(fetches), vis.Uses(vis.CPU))
	info, _ := vis.Lookup(k.Name())
	if !slices.Equal(info.Uses, []vis.Resource{vis.Memory, vis.CPU}) || !slices.Equal(info.Saves, []vis.Resource{vis.Resource(fetches.Name())}) {
		t.Fatalf("uses = %v, saves = %v", info.Uses, info.Saves)
	}
}

// A knob that saves a cost when turned up says so, rather than reading as one that uses it.
func TestAKnobThatSavesWhenTurnedUpSaysSo(t *testing.T) {
	k := vis.NewInt(fmt.Sprintf("test_loglevel_%d", lateN.Add(1)), vis.Range(-4, 8), vis.Default(0), vis.Saves(vis.CPU), vis.Uses(vis.Memory))
	info, _ := vis.Lookup(k.Name())
	if !slices.Equal(info.Uses, []vis.Resource{vis.Memory}) || !slices.Equal(info.Saves, []vis.Resource{vis.CPU}) {
		t.Fatalf("uses = %v, saves = %v; want memory used, cpu saved", info.Uses, info.Saves)
	}
}

// A cost can name a gauge declared after the knob, as vis gen's init does:
// its name is read when it's needed.
func TestACostNamesAGaugeDeclaredLater(t *testing.T) {
	var g vis.Gauge
	k := vis.NewInt(uniq("test_cost_knob"), vis.Range(1, 9), vis.Default(5), vis.Uses(&g))
	name := uniq("test_late_gauge")
	vis.DeclareGauge(&g, name)
	if info, _ := vis.Lookup(k.Name()); !slices.Equal(info.Uses, []vis.Resource{vis.Resource(name)}) {
		t.Fatalf("uses = %v, want the gauge's name", info.Uses)
	}
}

// A lever only people may pull says so: an agent offered it can only ask.
func TestALeverOnlyPeopleMayPullSaysSo(t *testing.T) {
	in := vis.NewInstance()
	for _, l := range in.Levers(vis.CPU) {
		if l.Knob == "test_people_lever" && !l.People {
			t.Fatalf("lever %+v: want it marked a person's", l)
		}
		if l.Knob == "test_slowness" && l.People {
			t.Fatalf("lever %+v: agents may pull it", l)
		}
	}
}

var _ = vis.NewInt("test_people_lever", vis.Range(1, 9), vis.Default(5), vis.Uses(vis.CPU))
