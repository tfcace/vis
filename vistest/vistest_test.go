package vistest_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/vistest"
)

var size = vis.NewInt("vistest_size", vis.Range(1, 100), vis.Default(10))

// Each test gets its own instance, so tests that change knobs run in parallel
// without seeing each other's values, or the default instance's.
func TestEachTestHasItsOwnInstance(t *testing.T) {
	for _, v := range []int{20, 30} {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			in := vistest.Start(t, vis.Service("test"))
			vistest.Set(t, in, size, v)
			if got := size.Get(in.Snapshot()); got != v {
				t.Fatalf("got %d, want %d", got, v)
			}
			if in.Service() != "test" {
				t.Fatalf("service = %q", in.Service())
			}
		})
	}
	if got := size.Now(); got != 10 {
		t.Fatalf("the default instance = %d, want its default untouched", got)
	}
}

// Ring starts a fleet in the test, joined before it returns, over an in-memory
// network: inside synctest its gossip and leases take no time.
func TestRingStartsAJoinedFleet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ring := vistest.Ring(t, 3, vis.Service("ringtest"))
		for _, in := range ring {
			if ms := in.Members(); len(ms) != 3 || ms[1].State != "alive" || ms[2].State != "alive" || ms[0].Service != "ringtest" {
				t.Fatalf("%s sees %+v, want three alive members of ringtest", ms[0].Name, ms)
			}
		}
		if err := ring[0].Freeze(vis.Writer{Kind: vis.Human, ID: "test"}, "hold"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		if !ring[2].FreezeState().Frozen {
			t.Fatal("a freeze on one member didn't reach another")
		}
	})
}
