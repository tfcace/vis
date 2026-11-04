package vis_test

// These tests came from a fifth review of the fleet layer, each a reproducer of a defect it found.

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

var ruleKnob = vis.NewInt("review5_rule_knob", vis.Range(1, 9), vis.Default(5), vis.Pull(vis.Rule))

// F4: a rule's reactions are kept out of last_change but fill the history it
// is read from: a person's change of an hour ago drops out, and the member
// reads as never changed.
func TestTheLastChangeSurvivesARuleAtWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 9, By: sam}); err != nil {
			t.Fatal(err)
		}
		r := vis.Writer{Kind: vis.Rule, ID: "autotune"}
		for i := 0; i < 1000; i++ { // one step every 4 s for 67 minutes
			if _, err := b.Set(vis.Change{Knob: "review5_rule_knob", Value: int64(1 + i%2), By: r, For: time.Hour}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(4 * time.Second)
		}
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, _ := call(t, cs, "fleet", nil)
		for _, m := range out["members"].([]any) {
			m := m.(map[string]any)
			if m["name"] == "b" {
				t.Logf("b: last_change=%v held=%v", m["last_change"], m["held"])
				if m["last_change"] == nil {
					t.Errorf("b holds sam's 9 but its last_change is empty: %s", fmt.Sprint(m))
				}
			}
		}
	})
}
