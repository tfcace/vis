package vis_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

func TestKnobsDescribeThemselves(t *testing.T) {
	k, ok := vis.Lookup("test_cache_size")
	if !ok {
		t.Fatal("test_cache_size not found")
	}
	if k.Name != "test_cache_size" || k.Type != "int" || k.Unit != "MiB" || k.Min != 1 || k.Max != 10000 || k.Default != 1000 {
		t.Fatalf("info = %+v", k)
	}
	if !slices.Equal(k.Pull, []vis.Kind{vis.Agent, vis.Rule, vis.Fleet}) {
		t.Fatalf("pull = %v, want agent, rule, fleet", k.Pull)
	}
	d, _ := vis.Lookup("test_timeout")
	if d.Type != "duration" || d.Format(d.Min) != "100ms" {
		t.Fatalf("duration info = %+v, min formats as %q", d, d.Format(d.Min))
	}
	if _, ok := vis.Lookup("nope"); ok {
		t.Fatal("unknown knob found")
	}
}

func TestKnobsSayWhatTheyDo(t *testing.T) {
	name := fmt.Sprintf("test_about_%d", lateN.Add(1))
	vis.NewInt(name, vis.Range(1, 9), vis.Default(6), vis.About("Compression level. Higher is smaller and slower."))
	if k, _ := vis.Lookup(name); k.About != "Compression level. Higher is smaller and slower." {
		t.Fatalf("about = %q", k.About)
	}
}

func TestKnobsListsEveryDeclarationInOrder(t *testing.T) {
	var names []string
	for _, k := range vis.Knobs() {
		names = append(names, k.Name)
	}
	i, j := slices.Index(names, "test_cache_size"), slices.Index(names, "test_timeout")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("knobs = %v, want test_cache_size before test_timeout", names)
	}
}

func TestValuesParseInTheKnobsOwnTerms(t *testing.T) {
	d, _ := vis.Lookup("test_timeout")
	if v, err := d.Parse("1.5s"); err != nil || time.Duration(v) != 1500*time.Millisecond {
		t.Fatalf("parse 1.5s = %v, %v", time.Duration(v), err)
	}
	if _, err := d.Parse("1500"); err == nil {
		t.Fatal("a bare number parsed as a duration; it should say to add a unit")
	}
	c, _ := vis.Lookup("test_cache_size")
	if v, err := c.Parse("2000"); err != nil || v != 2000 {
		t.Fatalf("parse 2000 = %d, %v", v, err)
	}
	if _, err := c.Parse("2k"); err == nil {
		t.Fatal("parsed 2k as an int")
	}
	if c.Format(2000) != "2000" {
		t.Fatalf("format = %q", c.Format(2000))
	}
}
