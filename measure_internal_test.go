package vis

import (
	"testing"

	"github.com/tfcace/vis/internal/spec"
)

// The library's ready-made measures are what the shared table says they are,
// so vis gen and vis vet, which read the table, agree with NewRule.
func TestTheMeasuresMatchTheSharedTable(t *testing.T) {
	byVar := map[string]Measure{"SchedP99": SchedP99, "GCCPU": GCCPU, "CPUThrottled": CPUThrottled, "CPUUsed": CPUUsed, "Goroutines": Goroutines}
	if len(spec.Measures) != len(byVar) {
		t.Fatalf("the table has %d measures, vis declares %d", len(spec.Measures), len(byVar))
	}
	for _, m := range spec.Measures {
		got, ok := byVar[m.Var]
		if !ok {
			t.Fatalf("the table names vis.%s, which vis doesn't declare", m.Var)
		}
		if got.Name != m.Name || got.kindOf() != m.Kind {
			t.Errorf("vis.%s is %q, %v; the table says %q, %v", m.Var, got.Name, got.kindOf(), m.Name, m.Kind)
		}
	}
	if RouteP99("GET /x").kindOf() != spec.RouteP99 {
		t.Error("RouteP99 disagrees with the table")
	}
	g, l := NewGauge("spec_gauge"), NewLatency("spec_latency")
	if g.Measure().kindOf() != spec.TypeKind("Gauge") || l.Measure().kindOf() != spec.TypeKind("Latency") {
		t.Errorf("gauge measures: %v, %v", g.Measure().kindOf(), l.Measure().kindOf())
	}
}
