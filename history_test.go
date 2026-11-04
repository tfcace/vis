package vis_test

import (
	"testing"
	"time"

	"github.com/tfcace/vis"
)

func TestHistoryKeepsWhatTheAuditTrailSays(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: time.Hour, By: claude, Reason: "hit rate 71%"})
	mustFreeze(t, in, sam, "investigating")
	h := in.History(10)
	if len(h) != 2 {
		t.Fatalf("history = %+v, want 2 events", h)
	}
	if e := h[0]; e.Kind != "change" || e.Fields["knob"] != "test_cache_size" || e.Fields["actor"] != "agent:claude" || e.Fields["reason"] != "hit rate 71%" || e.At.IsZero() {
		t.Fatalf("first event = %+v", e)
	}
	if e := h[1]; e.Kind != "freeze" || e.Fields["reason"] != "investigating" {
		t.Fatalf("second event = %+v", e)
	}
	if got := in.History(1); len(got) != 1 || got[0].Kind != "freeze" {
		t.Fatalf("History(1) = %+v, want only the newest", got)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	in := vis.NewInstance()
	for i := range vis.MaxHistory + 10 {
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: int64(1 + i%9000), By: sam})
	}
	if n := len(in.History(vis.MaxHistory * 2)); n != vis.MaxHistory {
		t.Fatalf("history holds %d events, want %d", n, vis.MaxHistory)
	}
}
