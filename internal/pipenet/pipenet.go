// Package pipenet is an in-memory network: listeners by address, and a dialer
// that reaches them over net.Pipe. testing/synctest counts a goroutine blocked
// on a pipe as durably blocked, so a ring of instances talking HTTP over it
// runs its timers (gossip, leases, cooldowns) in no time.
package pipenet

import (
	"context"
	"fmt"
	"net"
	"sync"
)

// Network is a set of listeners, by address.
type Network struct {
	mu        sync.Mutex
	listeners map[string]*Listener
}

// New returns an empty network.
func New() *Network { return &Network{listeners: map[string]*Listener{}} }

// Listen takes addr on the network.
func (n *Network) Listen(addr string) (*Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.listeners[addr]; ok {
		return nil, fmt.Errorf("pipenet: %s is in use", addr)
	}
	l := &Listener{net: n, addr: addr, conns: make(chan net.Conn), done: make(chan struct{})}
	n.listeners[addr] = l
	return l, nil
}

// Dial connects to the listener at addr, as net.Dialer.DialContext would.
func (n *Network) Dial(ctx context.Context, _, addr string) (net.Conn, error) {
	n.mu.Lock()
	l := n.listeners[addr]
	n.mu.Unlock()
	if l == nil {
		return nil, &net.OpError{Op: "dial", Net: "pipe", Err: fmt.Errorf("connection refused: nothing listens on %s", addr)}
	}
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		return nil, &net.OpError{Op: "dial", Net: "pipe", Err: fmt.Errorf("connection refused: %s closed", addr)}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Listener is one address on a Network.
type Listener struct {
	net   *Network
	addr  string
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

// Accept waits for the next connection.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close frees the address; dials to it are refused from now on.
func (l *Listener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.net.mu.Lock()
		delete(l.net.listeners, l.addr)
		l.net.mu.Unlock()
	})
	return nil
}

// Addr is the listener's address.
func (l *Listener) Addr() net.Addr { return addr(l.addr) }

type addr string

func (a addr) Network() string { return "pipe" }
func (a addr) String() string  { return string(a) }
