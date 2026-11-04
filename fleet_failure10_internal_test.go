package vis

// These tests came from what agents asked for in the evals.

import (
	"strings"
	"testing"
	"time"
)

// A fix whose lease ended on every member at about once says so in what the
// fleet shares: in fleet-relapse, agents found it in each member's last
// change and asked to be told directly.
func TestALeaseThatEndedOnEveryMemberIsShared(t *testing.T) {
	ms := []FleetMember{{Member: Member{Name: "cp-1"}}, {Member: Member{Name: "cp-2"}}, {Member: Member{Name: "cp-3"}}}
	ended := "compress_level 1 → 9: human:sam's lease ended"
	sums := []*peerSummary{{Last: ended, LastAgo: 70 * time.Second}, {Last: ended, LastAgo: 71 * time.Second}, {Last: ended, LastAgo: 69 * time.Second}}
	c := strings.Join(common(ms, sums), "; ")
	if !strings.Contains(c, "human:sam's lease ended on every member") || !strings.Contains(c, "compress_level") {
		t.Errorf("common = %q; want the lease that ended everywhere named", c)
	}
	sums[1] = &peerSummary{Last: "compress_level 9 → 1 by agent:oncall", LastAgo: 10 * time.Second}
	if c := strings.Join(common(ms, sums), "; "); strings.Contains(c, "lease ended") {
		t.Errorf("common = %q; one member was changed since, so it isn't every member's", c)
	}
}
