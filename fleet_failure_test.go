package vis_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// These tests came from a review of the fleet layer, each a reproducer of a defect it found.

// cut lets a test sever links: dialer(from) refuses to reach a cut peer.
type cut struct {
	n   *pipenet.Network
	off atomic.Bool
}

func (c *cut) dialer(from string) vis.ServeOption {
	return vis.PeerDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		if c.off.Load() && (from == "b" && strings.HasPrefix(addr, "c:") || from == "c" && strings.HasPrefix(addr, "b:")) {
			return nil, &net.OpError{Op: "dial", Net: "pipe", Err: errors.New("partitioned")}
		}
		conn, err := c.n.Dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if from == "b" && strings.HasPrefix(addr, "c:") || from == "c" && strings.HasPrefix(addr, "b:") {
			return &cutConn{Conn: conn, c: c}, nil
		}
		return conn, nil
	})
}

// cutConn fails once the link is cut, as a connection across a partition would.
type cutConn struct {
	net.Conn
	c *cut
}

func (k *cutConn) Write(p []byte) (int, error) {
	if k.c.off.Load() {
		k.Conn.Close()
		return 0, errors.New("partitioned")
	}
	return k.Conn.Write(p)
}

// The seed crashes; b and c lose each other for 20 s; the network heals. Do they find each other again?
func TestAPartitionHealsWithoutTheSeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		c := &cut{n: n}
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", c.dialer("a"))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", c.dialer("b"))
		_, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946", c.dialer("c"))
		defer func() { stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.SkipLeave(a)
		stopA()
		time.Sleep(3 * time.Second)
		c.off.Store(true)
		time.Sleep(20 * time.Second)
		c.off.Store(false)
		time.Sleep(60 * time.Second)
		got := members(b)
		t.Logf("b, a minute after the network healed: %v", got)
		if !slices.Contains(got, "c:alive") {
			t.Errorf("b never found c again: %v", got)
		}
	})
}

// Sam unfreezes on a; a second later an agent freezes on b, which hasn't heard yet.
func TestAFreezeDuringAnUnfreezeHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		cc, stopC := peer(t, n, "cacheproxy", "c", "k", "a:7946")
		defer func() { stopA(); stopB(); stopC(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		a.Freeze(sam, "incident")
		time.Sleep(3 * time.Second)
		a.Unfreeze(sam, "looks fine")
		err := b.Freeze(claude, "the rollout is hurting p99: hold everything")
		t.Logf("b.Freeze = %v; b right after: %+v", err, b.FreezeState())
		time.Sleep(3 * time.Second)
		for _, in := range []*vis.Instance{a, b, cc} {
			if !in.FreezeState().Frozen {
				t.Errorf("%s: unfrozen; the agent's freeze, which returned %v, is gone", in.Members()[0].Name, err)
			}
		}
	})
}

// A member that accepts but doesn't answer, with FleetWait raised as the docs advise:
// how long does this instance's own page take, and how often does its stream speak?
func TestAStalledMemberDoesntStallThePage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.FleetWait(10*time.Second))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stopB(); synctest.Wait() }()
		time.Sleep(3 * time.Second)
		vis.StallSummaries(b, time.Hour)
		ln, _ := n.Listen("a:7071")
		srv := &http.Server{Handler: a.UI(vis.UIUser("sam"))}
		go srv.Serve(ln)
		defer srv.Close()
		hc := &http.Client{Transport: &http.Transport{DialContext: n.Dial}}
		defer hc.CloseIdleConnections()
		get := func(path string) *http.Response {
			req, _ := http.NewRequest("GET", "http://a:7071"+path, nil)
			req.Host = "localhost:7071"
			res, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		start := time.Now()
		res := get("/api/state")
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		t.Logf("/api/state took %v", time.Since(start))

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://a:7071/api/events", nil)
		req.Host = "localhost:7071"
		sres, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		events := 0
		buf := make([]byte, 1<<20)
		for {
			k, err := sres.Body.Read(buf)
			events += strings.Count(string(buf[:k]), "event: state")
			if err != nil {
				break
			}
		}
		sres.Body.Close()
		t.Logf("events in 60 s: %d (every 2 s would be about 30)", events)
		if events < 25 {
			t.Errorf("the page's stream spoke %d times in 60 s", events)
		}
	})
}

// After 10 minutes a dead member is forgotten. Does it stay forgotten, when a
// member that joined later relays it?
func TestAForgottenMemberStaysForgotten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		d, stopD := peer(t, n, "cacheproxy", "d", "k", "a:7946")
		time.Sleep(3 * time.Second)
		vis.SkipLeave(d)
		stopD()
		time.Sleep(30 * time.Second)
		e, stopE := peer(t, n, "cacheproxy", "e", "k", "a:7946")
		defer func() { stopA(); stopB(); stopE(); synctest.Wait() }()
		for _, at := range []time.Duration{11 * time.Minute, 25 * time.Minute, 45 * time.Minute} {
			time.Sleep(at - time.Duration(0))
			for _, in := range []*vis.Instance{a, b, e} {
				if got := members(in); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "d:") }) {
					t.Errorf("%s still lists d: %v", got[0], got)
				}
			}
		}
	})
}
