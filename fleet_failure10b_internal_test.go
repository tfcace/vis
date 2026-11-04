package vis

// These tests came from a tenth review of the fleet layer, each a reproducer of a defect it found.

import (
	"strings"
	"testing"
	"time"
)

func TestTheLeaseLineCountsWhoAnswered(t *testing.T) {
	ms := []FleetMember{{Member: Member{Name: "cp-1"}}, {Member: Member{Name: "cp-2"}}, {Member: Member{Name: "cp-3"}}}
	ended := "compress_level 1 → 9: human:sam's lease ended"
	s := func(last string, ago time.Duration) *peerSummary { return &peerSummary{Last: last, LastAgo: ago} }
	cases := map[string]struct {
		ms   []FleetMember
		sums []*peerSummary
		want string // in the lease line, or "" for none
	}{
		"third silent":              {[]FleetMember{ms[0], ms[1], {Member: Member{Name: "cp-3"}, Silent: true}}, []*peerSummary{s(ended, 70*time.Second), s(ended, 71*time.Second), nil}, "every member that answered (2 of 3), about 1m ago"},
		"third gossip-only":         {[]FleetMember{ms[0], ms[1], {Member: Member{Name: "cp-3"}, Gossiped: 1}}, []*peerSummary{s(ended, 70*time.Second), s(ended, 71*time.Second), s(ended, 72*time.Second)}, "on every member, about 1m ago"},
		"third not answering (nil)": {ms, []*peerSummary{s(ended, 70*time.Second), s(ended, 71*time.Second), nil}, "every member that answered (2 of 3)"},
		"two of three ended":        {ms, []*peerSummary{s(ended, 70*time.Second), s(ended, 71*time.Second), s("compress_level 9 → 1 by agent:x", 10*time.Second)}, ""},
	}
	for name, c := range cases {
		got := strings.Join(common(c.ms, c.sums), "; ")
		if c.want == "" && strings.Contains(got, "lease ended") || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: common = %q; want %q", name, got, c.want)
		}
	}
}
