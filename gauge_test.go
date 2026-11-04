package vis_test

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

// A gauge is declared like a knob and set wherever the app computes the value;
// every instance publishes it.
func TestAGaugeIsDeclaredAndSet(t *testing.T) {
	name := uniq("test_fetches")
	g := vis.NewGauge(name, vis.Unit("images/s"), vis.About("Images fetched from origins per second."))
	in := vis.NewInstance()
	if s, ok := signalNamed(in, name); !ok || s.Err == "" {
		t.Fatalf("before Set = %+v, %v; want it listed, with no value yet", s, ok)
	}
	g.Set(3.5)
	if s, _ := signalNamed(in, name); s.Value != 3.5 || s.Unit != "images/s" || s.About == "" || s.Err != "" {
		t.Fatalf("after Set = %+v", s)
	}
	if m := g.Measure(); m.Name != name || m.Read(in) != 3.5 {
		t.Fatalf("as a measure = %+v", m)
	}
}

// A latency gauge takes a time.Duration and publishes milliseconds.
func TestALatencyGaugeTakesADuration(t *testing.T) {
	name := uniq("test_wait")
	g := vis.NewLatency(name, vis.About("How long misses wait."))
	g.Set(42 * time.Millisecond)
	if s, _ := signalNamed(vis.NewInstance(), name); s.Value != 42 || s.Unit != "ms" {
		t.Fatalf("latency = %+v", s)
	}
}

// A value computed when read is a GaugeFunc.
func TestAGaugeFuncIsReadWhenAsked(t *testing.T) {
	name := uniq("test_live")
	var n atomic.Int64 // other tests' instances read every declared gauge too
	vis.NewGaugeFunc(name, func() float64 { return float64(n.Add(1)) })
	in := vis.NewInstance()
	first, _ := signalNamed(in, name)
	if s, _ := signalNamed(in, name); s.Value <= first.Value {
		t.Fatalf("read %v then %v, want it read afresh on each ask", first.Value, s.Value)
	}
}

// Options that bound a knob make no sense on a gauge: the declaration says so.
func TestAGaugeRefusesKnobOptions(t *testing.T) {
	defer func() {
		if p := fmt.Sprint(recover()); !strings.Contains(p, "Range") {
			t.Fatalf("panic = %q, want it to name the option", p)
		}
	}()
	vis.NewGauge(uniq("test_bad"), vis.Range(1, 2))
}

// A gauge computed when read has a handle: a rule can watch it, a cost can name it.
func TestAGaugeFuncHasAHandle(t *testing.T) {
	name := uniq("test_live_rate")
	g := vis.NewGaugeFunc(name, func() float64 { return 3 })
	k := vis.NewInt(uniq("test_gf_knob"), vis.Range(1, 9), vis.Default(5), vis.Saves(g))
	if g.Name() != name || g.Measure().Read(vis.NewInstance()) != 3 {
		t.Fatalf("handle = %q, %v", g.Name(), g.Measure().Read(vis.NewInstance()))
	}
	if info, _ := vis.Lookup(k.Name()); !slices.Equal(info.Saves, []vis.Resource{vis.Resource(name)}) {
		t.Fatalf("saves = %v", info.Saves)
	}
}
