// Package vistest helps test code that uses vis: each test gets its own
// instance, so tests that change knobs can run in parallel.
package vistest

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// Start gives the test its own instance, with opts. Use it instead of the
// default instance, which every test in the process shares.
func Start(t testing.TB, opts ...vis.InstanceOption) *vis.Instance {
	t.Helper()
	return vis.NewInstance(opts...)
}

// Set pins an Int knob to v on in, as a person would, failing the test if it can't.
func Set(t testing.TB, in *vis.Instance, k vis.Int, v int) {
	t.Helper()
	if _, err := in.Apply(vis.Tx{By: vis.Writer{Kind: vis.Human, ID: "test"}, Reason: t.Name(),
		Set: []vis.Write{{Knob: k.Name(), Value: int64(v)}}}); err != nil {
		t.Fatalf("vistest: set %s to %d: %v", k.Name(), v, err)
	}
}

// Ring starts a fleet of n instances of one service, with opts, joined into a
// ring over an in-memory network before it returns, and stops them when the
// test ends. They are named i0, i1 and on. Inside testing/synctest the ring's
// gossip, leases and cooldowns run in no time.
func Ring(t testing.TB, n int, opts ...vis.InstanceOption) []*vis.Instance {
	t.Helper()
	net := pipenet.New()
	key := rand.Text()
	ins := make([]*vis.Instance, n)
	for i := range n {
		name := fmt.Sprintf("i%d", i)
		ln, err := net.Listen(name + ":7946")
		if err != nil {
			t.Fatalf("vistest: %v", err)
		}
		in := vis.NewInstance(append([]vis.InstanceOption{vis.Service("test"), vis.InstanceName(name)}, opts...)...)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(func() { cancel(); in.Stop() }) // Stop waits until it has left
		if _, err := in.Serve(ctx, vis.MCPAt(""), vis.UIAt(""), vis.ClusterKey(key), vis.Seeds("i0:7946"),
			vis.PeerListener(ln), vis.PeerDialer(net.Dial), vis.PeerAdvertise(name+":7946")); err != nil {
			t.Fatalf("vistest: %v", err)
		}
		ins[i] = in
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		joined := true
		for _, in := range ins {
			alive := 0
			for _, m := range in.Members() {
				if m.State == "alive" {
					alive++
				}
			}
			joined = joined && alive == n
		}
		if joined {
			return ins
		}
		if time.Now().After(deadline) {
			t.Fatalf("vistest: the ring of %d didn't join in 30 s", n)
		}
	}
}
