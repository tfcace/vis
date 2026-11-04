package vis

import (
	"bytes"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tfcace/vis/internal/pipenet"
)

// bareRing wires a ring on in without gossip, for poking at one function.
// These tests came from a review of the fleet layer, each a reproducer of a defect it found.
func bareRing(t *testing.T, in *Instance, n *pipenet.Network) *ring {
	r := &ring{in: in, key: []byte("k"), client: &http.Client{Transport: &http.Transport{DialContext: n.Dial}, Timeout: time.Second},
		members: map[string]*memberEntry{}, left: map[string]time.Time{}, seedErr: map[string]string{}, wait: 200 * time.Millisecond}
	r.self = wireMember{ID: in.id, Name: in.name, Addr: "a:7946", Service: in.service}
	in.ring.Store(r)
	return r
}

// A member whose clock runs 30 s ahead: its digest, truly 4 s old, reads as 1 ms old.
func TestGossipedAgeIsJudgedByThisClock(t *testing.T) {
	n := pipenet.New()
	in := NewInstance(Service("s"), InstanceName("a"))
	r := bareRing(t, in, n)
	ln, _ := n.Listen("b:7946")
	go func() { // accepts, never answers: a starved member
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				b := make([]byte, 1<<16)
				for {
					if _, err := c.Read(b); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	defer ln.Close()
	r.members["b"] = &memberEntry{m: wireMember{ID: "b", Name: "b", Addr: "b:7946", Service: "s",
		Health: &wireHealth{At: time.Now().Add(30*time.Second - 4*time.Second).UnixNano()}}, rose: time.Now().Add(-4 * time.Second)}
	rep := in.Fleet(t.Context())
	for _, m := range rep.Members {
		if m.Name == "b" {
			t.Logf("b: gossiped=%v silent=%v covers=%q", m.Gossiped, m.Silent, rep.Covers)
			if m.Gossiped < 3*time.Second {
				t.Errorf("b's numbers read as %v old; they are 4 s old (its clock is 30 s ahead)", m.Gossiped)
			}
		}
	}
}

func signed(r *ring, path string, body []byte, as string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.sign(req, body)
	if as != "" {
		req.Header.Set("Vis-UI-As", as)
	}
	return req
}

// The acting person rides outside the signature, and a signed request can be replayed.
func TestTheActingPersonIsSignedAndNotReplayable(t *testing.T) {
	n := pipenet.New()
	in := NewInstance(Service("s"), InstanceName("a"))
	r := bareRing(t, in, n)
	body := []byte(`{"reason":"hold"}`)
	orig := signed(r, peerPath+"ui/api/freeze", body, "human:sam")
	// An observer copies its signature headers onto a request naming someone else.
	forged := httptest.NewRequest(http.MethodPost, peerPath+"ui/api/freeze", bytes.NewReader(body))
	for _, h := range []string{"Vis-Peer-Time", "Vis-Peer-Nonce", "Vis-Peer-Service", "Vis-Peer-Sig"} {
		forged.Header.Set(h, orig.Header.Get(h))
	}
	forged.Header.Set("Vis-UI-As", "human:mallory")
	w := httptest.NewRecorder()
	r.serve(w, forged)
	f := in.FreezeState()
	t.Logf("forged: %d, freeze by %v", w.Code, f.By)
	if w.Code == 200 && f.By.ID == "mallory" {
		t.Errorf("a signature made for sam's request froze the instance as mallory")
	}
	// Replay: the same unfreeze, twice, both accepted (within 60 s).
	ub := []byte(`{"reason":"done"}`)
	u1 := signed(r, peerPath+"ui/api/unfreeze", ub, "human:sam")
	u2 := u1.Clone(u1.Context())
	u2.Body = io_nop(ub)
	w1 := httptest.NewRecorder()
	r.serve(w1, u1)
	in.Freeze(Writer{Kind: Human, ID: "sam"}, "again")
	w2 := httptest.NewRecorder()
	r.serve(w2, u2)
	t.Logf("replayed unfreeze: %d then %d, frozen now %v", w1.Code, w2.Code, in.FreezeState().Frozen)
	if w2.Code == 200 && !in.FreezeState().Frozen {
		t.Errorf("a replayed unfreeze undid a later freeze")
	}
}

// One peer whose clock is off by a drifting amount fills the refusal list, and a key mismatch goes unreported.
func TestAWrongKeyIsReportedWhateverElseIsRefused(t *testing.T) {
	n := pipenet.New()
	in := NewInstance(Service("s"), InstanceName("a"))
	r := bareRing(t, in, n)
	for i := range 8 {
		req := httptest.NewRequest(http.MethodPost, peerPath+"gossip", strings.NewReader("{}"))
		at := strconv.FormatInt(time.Now().Add(-time.Duration(61+i)*time.Second).Unix(), 10)
		req.Header.Set("Vis-Peer-Time", at)
		req.Header.Set("Vis-Peer-Service", "s")
		req.Header.Set("Vis-Peer-Sig", r.mac(at, req.URL.Path, []byte("{}")))
		r.serve(httptest.NewRecorder(), req)
	}
	other := &ring{key: []byte("wrong"), self: wireMember{Service: "s"}}
	req := httptest.NewRequest(http.MethodPost, peerPath+"gossip", strings.NewReader("{}"))
	other.sign(req, []byte("{}"))
	r.serve(httptest.NewRecorder(), req)
	got := strings.Join(r.refusals(), "\n")
	t.Logf("refusals:\n%s", got)
	if !strings.Contains(got, "VIS_CLUSTER_KEY") {
		t.Errorf("a peer with the wrong key went unreported")
	}
}

// Replies aren't verified: whoever answers at a member's address can set the
// freeze epoch to the top, after which a freeze has epoch 0 and never spreads.
func TestAKeylessReplyCantPinTheEpoch(t *testing.T) {
	n := pipenet.New()
	in := NewInstance(Service("s"), InstanceName("a"))
	r := bareRing(t, in, n)
	ln, _ := n.Listen("b:7946")
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { // no key at all
		json.NewEncoder(w).Encode(gossipMsg{Members: []wireMember{{ID: "x", Name: "x", Addr: "b:7946", Service: "s", Heartbeat: 1,
			Freeze: &wireFreeze{Frozen: false, By: "human:x", Epoch: math.MaxUint64, Origin: "zzzz"}}}})
	}))
	defer ln.Close()
	r.send(t.Context(), "b:7946")
	in.Freeze(Writer{Kind: Human, ID: "sam"}, "hold")
	f := in.FreezeState()
	t.Logf("after a keyless reply: epoch %d frozen %v; on the wire: %+v; members: %d", f.Epoch, f.Frozen, freezeWire(f), len(in.Members()))
	if freezeWire(f) == nil {
		t.Errorf("sam's freeze has epoch 0 and won't spread")
	}
}

type nopBody struct{ *bytes.Reader }

func (nopBody) Close() error  { return nil }
func io_nop(b []byte) nopBody { return nopBody{bytes.NewReader(b)} }
