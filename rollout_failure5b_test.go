package vis_test

// These tests came from a fifth review of the fleet layer, each a reproducer of a defect it found.

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

// handshakeCut loses the member's answer to the MCP initialize request, as a
// member restarting (or a reset connection) between accept and answer would.
// The tool call is never sent.
type handshakeCut struct {
	net.Conn
	armed *atomic.Bool
	drop  atomic.Bool
}

func (c *handshakeCut) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"server/discover"`)) && c.armed.CompareAndSwap(true, false) {
		c.drop.Store(true)
	}
	return c.Conn.Write(p)
}

func (c *handshakeCut) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.drop.Load() {
		c.Conn.Close()
		return 0, io.EOF
	}
	return n, err
}

// F3: a handshake that fails other than by timing out (EOF, reset, an HTTP
// error) is read as "the call reached it": stage 1 fails the whole rollout,
// telling the operator the member may have taken a change it was never sent.
func TestAFailedHandshakeIsNotALostAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		o := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		var armed atomic.Bool
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := n.Dial(ctx, network, addr)
			if err != nil || addr != o[0]+":7946" {
				return c, err
			}
			return &handshakeCut{Conn: c, armed: &armed}, nil
		}
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.PeerDialer(dial))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		armed.Store(true)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 3}, "for": "10m", "reason": "x"})
		t.Logf("order=%v isErr=%v outcome=%v why=%v", o, isErr, out["outcome"], out["why"])
		ins := map[string]*vis.Instance{"a": a, "b": b, "c": c}
		t.Logf("%s=%d (never sent the call)", o[0], fleetKnob.Get(ins[o[0]].Snapshot()))
		if strings.Contains(fmt.Sprint(out["why"]), "may have taken") {
			t.Errorf("a member that never got the call is said to maybe hold the change: %v", out["why"])
		}
	})
}

// handshakeCutNth cuts the member's answer to the nth handshake on any of its
// connections, and no other.
type handshakeCutNth struct {
	net.Conn
	seen *atomic.Int32
	nth  int32
	drop atomic.Bool
}

func (c *handshakeCutNth) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"server/discover"`)) && c.seen.Add(1) == c.nth {
		c.drop.Store(true)
	}
	return c.Conn.Write(p)
}

func (c *handshakeCutNth) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.drop.Load() {
		c.Conn.Close()
		return 0, io.EOF
	}
	return n, err
}

// Measuring the first stage is a read that was never sent when its
// handshake fails: it is tried once more, rather than stopping the rollout
// as unjudged. In fleet-relapse a starved member answered a handshake with
// 400 Bad Request, and the rollout stopped; the agent changed the rest by hand.
func TestAMeasurementWhoseHandshakeFailedIsTriedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		slo := vis.SLO("GET /fleet/{k}", 50*time.Millisecond)
		o := vis.StageOrder([]string{"a", "b", "c"}, "fleet_test_knob")
		var seen atomic.Int32
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := n.Dial(ctx, network, addr)
			if err != nil || addr != o[0]+":7946" {
				return c, err
			}
			return &handshakeCutNth{Conn: c, seen: &seen, nth: 2}, nil // the first is set_knob's, the second impact's
		}
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", slo, vis.PeerDialer(dial))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", slo)
		c, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", slo)
		ctx, cancel := context.WithCancel(context.Background())
		var load sync.WaitGroup
		defer func() {
			cancel()
			load.Wait()
			stopA()
			stopB()
			stopC()
			synctest.Wait()
		}()
		fleetLoad(ctx, &load, func(v int) time.Duration {
			if v <= 2 {
				return 10 * time.Millisecond
			}
			return 200 * time.Millisecond
		}, a, b, c)
		time.Sleep(65 * time.Second)
		seen.Store(0)
		cs := connectOver(t, n, a, "a:7070", "oncall-secret")
		out, isErr := call(t, cs, "rollout", map[string]any{"set": map[string]any{"fleet_test_knob": 2}, "for": "10m", "reason": "x"})
		if isErr || out["outcome"] != "done" {
			t.Fatalf("rollout after a broken measurement handshake: %v", out)
		}
	})
}
