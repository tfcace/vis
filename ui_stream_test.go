package vis_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

// streamRec is a ResponseWriter that keeps what a stream wrote, flushed or not.
type streamRec struct {
	mu   sync.Mutex
	hdr  http.Header
	body bytes.Buffer
}

func (s *streamRec) Header() http.Header { return s.hdr }
func (s *streamRec) WriteHeader(int)     {}
func (s *streamRec) Flush()              {}
func (s *streamRec) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(b)
}

// states is every state event's data so far, decoded.
func (s *streamRec) states(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, ev := range strings.Split(s.body.String(), "\n\n") {
		if !strings.HasPrefix(ev, "event: state\n") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "event: state\ndata: ")), &m); err != nil {
			t.Fatalf("event %q: %v", ev, err)
		}
		out = append(out, m)
	}
	return out
}

func knobValue(st map[string]any, name string) any {
	for _, k := range st["knobs"].([]any) {
		if k := k.(map[string]any); k["name"] == name {
			return k["value"]
		}
	}
	return nil
}

func historyLen(st map[string]any) int {
	return len(st["routes"].([]any)[0].(map[string]any)["history"].([]any))
}

// The page keeps one connection open: a whole snapshot first, then a change as
// soon as it happens (a burst coalesced over 50 ms), and the measurements every
// 2 s, each carrying only the newest buckets of a route's hour.
func TestTheUIStreamsStateAsItChanges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance(vis.SLO("GET /k", 50*time.Millisecond))
		stop := slowService(t, in)
		defer stop()
		time.Sleep(35 * time.Second) // a few closed buckets of history

		ctx, cancel := context.WithCancel(context.Background())
		rec := &streamRec{hdr: http.Header{}}
		done := make(chan struct{})
		go func() {
			defer close(done)
			req := httptest.NewRequest("GET", "http://localhost/api/events", nil).WithContext(ctx)
			in.UI(vis.UIUser("sam")).ServeHTTP(rec, req)
		}()
		synctest.Wait()
		if ct := rec.hdr.Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("content type %q", ct)
		}
		if !strings.HasPrefix(rec.body.String(), "retry: 2000\n\n") {
			t.Fatalf("stream starts %q, want a reconnect delay", rec.body.String()[:min(40, rec.body.Len())])
		}
		first := rec.states(t)
		if len(first) != 1 || historyLen(first[0]) < 3 {
			t.Fatalf("on connect: %d events, want one whole snapshot", len(first))
		}

		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: sam})
		time.Sleep(40 * time.Millisecond)
		synctest.Wait()
		if n := len(rec.states(t)); n != 1 {
			t.Fatalf("%d events 40 ms after a change, want it held back to coalesce a burst", n)
		}
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		got := rec.states(t)
		if len(got) != 2 || knobValue(got[1], "test_cache_size") != 2000.0 {
			t.Fatalf("60 ms after a change: %d events, want the change", len(got))
		}
		if n := historyLen(got[1]); n > 2 {
			t.Fatalf("a change event carried %d buckets, want only the newest two", n)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := len(rec.states(t)); n != 3 {
			t.Fatalf("%d events after 2 s more, want the measurements' tick", n)
		}

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the stream outlived its request")
		}
	})
}
