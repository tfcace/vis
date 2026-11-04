package vis_test

// These tests came from a fourth review of the fleet layer, each a reproducer of a defect it found.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

var dialKnob = vis.NewInt("upstream_dial_ms", vis.Range(1, 9), vis.Default(5), vis.Pull(vis.Agent))

// R1: a knob whose name contains "dial" turns every member's refusal into
// "couldn't be reached": a value out of range reads as a higher layer holding it.
func TestAKnobNamedDialStillReportsARefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		_, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		_, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"upstream_dial_ms": 50}, "for": "10m", "reason": "out of range"})
		t.Logf("isErr=%v out=%v", isErr, out)
		if !strings.Contains(fmt.Sprint(out), "out of range") {
			t.Errorf("an out-of-range value isn't reported as such: %v", out)
		}
	})
}

// R2: the only reachable member already holds the value, the other can't be
// reached: the rollout says a higher layer holds the knob everywhere, and
// doesn't name the member it never reached.
func TestAnUnreachedMemberIsNamedWhenNothingElseTakesEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { vis.StallPeers(b, 0); stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		if out, isErr := call(t, cs, "set_knob", map[string]any{"instance": "a", "set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "already"}); isErr {
			t.Fatalf("set_knob: %v", out)
		}
		vis.StallPeers(b, time.Hour)
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "b starved"})
		vis.StallPeers(b, 0)
		t.Logf("isErr=%v out=%v", isErr, out)
		if fleetKnob.Get(b.Snapshot()) != 2 && !strings.Contains(fmt.Sprint(out["not_reached"]), "b") {
			t.Errorf("b was never changed and the rollout doesn't say so: %v", out)
		}
	})
}

// dropConn passes a set_knob request through to the member, then loses the
// member's answer, as a connection cut after the member applied the change would.
type dropConn struct {
	net.Conn
	armed *atomic.Bool
	drop  atomic.Bool
}

func (c *dropConn) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"set_knob"`)) && c.armed.CompareAndSwap(true, false) {
		c.drop.Store(true)
	}
	return c.Conn.Write(p)
}

func (c *dropConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.drop.Load() {
		c.Conn.Close()
		return 0, io.EOF
	}
	return n, err
}

// R3: a member whose name contains "dial" applies the change in stage 1, the
// answer is lost (EOF), and the rollout takes it for one it never reached: it
// moves on, the next member is harmed and undone, and the rollout says the rest
// were left alone while radial holds the change under a lease nobody named.
func TestALostAnswerIsNotTakenForAnUnreachedMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		var armed atomic.Bool
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := n.Dial(ctx, network, addr)
			if err != nil || addr != "radial:7946" {
				return c, err
			}
			return &dropConn{Conn: c, armed: &armed}, nil
		}
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo, vis.PeerDialer(dial))
		r, stopR := peer(t, n, "cacheproxy", "radial", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopR()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(v int) time.Duration {
			switch {
			case v <= 2:
				return 10 * time.Millisecond
			case v == 3:
				return 400 * time.Millisecond
			}
			return 30 * time.Millisecond
		}, a, r, c)
		time.Sleep(65 * time.Second)
		if o := vis.StageOrder([]string{"a", "radial", "c"}, "fleet_test_knob"); o[0] != "radial" {
			t.Fatalf("order %v", o)
		}
		armed.Store(true)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 3}, "for": "10m", "reason": "a bad change"})
		t.Logf("isErr=%v out=%v", isErr, out)
		if out["outcome"] != "failed" || !strings.Contains(fmt.Sprint(out["why"]), "radial") || !strings.Contains(fmt.Sprint(out["why"]), "may have") {
			t.Errorf("a lost answer from radial: %v; want failed, naming radial, saying it may have taken the change", out)
		}
		if fleetKnob.Get(a.Snapshot()) != 5 || fleetKnob.Get(c.Snapshot()) != 5 {
			t.Errorf("a=%d c=%d: the rollout went on past a member whose answer it lost", fleetKnob.Get(a.Snapshot()), fleetKnob.Get(c.Snapshot()))
		}
	})
}
