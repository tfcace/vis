package vis_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
	"github.com/tfcace/vis/internal/pipenet"
)

// A container at 500 of its 512 MiB, as the memory evals' cases were moments
// before each OOM kill.
func nearItsLimit(t *testing.T) {
	vis.FakeCgroup(t, map[string]string{
		"cpu.max": "max 100000\n", "cpu.stat": "usage_usec 0\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n",
		"memory.current": "524288000\n", "memory.max": "536870912\n", "memory.stat": "inactive_file 0\n", "memory.events": "oom_kill 0\n",
	})
}

// deathStory is the instance's story about a member that died, if it tells one.
func deathStory(in *vis.Instance, member string) (vis.Story, bool) {
	for _, s := range in.Stories(20, sam) {
		if strings.Contains(s.Title, member) && strings.Contains(s.Title, "died") || member == "" && strings.Contains(s.Title, "died") {
			return s, true
		}
	}
	return vis.Story{}, false
}

// A member killed without a word (an OOM kill gives none) is told about when
// it comes back under its name: by its peers, which kept its last digest, so
// no disk is needed. The restarted member hears what killed it most likely
// and what it held that the kill dropped; its peers say the same about it.
// In the memory evals' case c the kill took away the value keeping the service up.
func TestAMemberThatDiedIsToldWhyAndWhatItLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		time.Sleep(3 * time.Second)
		if _, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: time.Hour, Reason: "fits in memory"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second) // its digest carries what it holds, and its memory
		vis.SkipLeave(b)            // killed: no word to the fleet
		stopB()
		time.Sleep(time.Second)
		b2, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stop(); synctest.Wait() }()
		time.Sleep(10 * time.Second)

		for who, in := range map[string]*vis.Instance{"the restarted b": b2, "its peer a": a} {
			s, ok := deathStory(in, "b")
			if !ok {
				t.Fatalf("%s tells no story of b's death: %+v", who, in.Stories(20, sam))
			}
			t.Logf("%s: %s\n%s", who, s.Title, s.Body)
			if !s.Alert {
				t.Errorf("%s: the death isn't an alert: %+v", who, s)
			}
			has(t, s.Title, "most likely killed for memory")
			has(t, s.Body, "500 of 512 MiB")
			has(t, s.Body, "fleet_test_knob 8")
			has(t, s.Body, "agent:claude")
		}
	})
}

// A member that stopped cleanly, as a deploy stops it, didn't die: nobody
// tells a death story.
func TestACleanStopIsNoDeath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "")
		_, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		time.Sleep(3 * time.Second)
		stopB()
		time.Sleep(time.Second)
		b2, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946")
		defer func() { stopA(); stop(); synctest.Wait() }()
		time.Sleep(10 * time.Second)
		for _, in := range []*vis.Instance{a, b2} {
			if s, ok := deathStory(in, "b"); ok {
				t.Fatalf("a clean stop told as a death: %+v", s)
			}
		}
	})
}

// alone serves an instance with no fleet and a note in dir, as vis.Start does
// a service's (in the temp dir unless VIS_STATE_DIR says where).
func alone(t *testing.T, dir string) (*vis.Instance, func()) {
	t.Helper()
	in := vis.NewInstance(vis.Service("cacheproxy"), vis.InstanceName("solo"))
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := in.Serve(ctx, vis.MCPAt(""), vis.UIAt(""), vis.StateDir(dir)); err != nil {
		cancel()
		t.Fatal(err)
	}
	return in, func() { cancel(); in.Stop() }
}

// Alone, with no peers to keep its last word, an instance keeps it in a file
// every couple of seconds; the process that replaces it reads it and tells the
// death, as a fleet would.
func TestAnInstanceAloneIsToldWhyItsFormerSelfDied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		if _, err := a.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: time.Hour, Reason: "fits in memory"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second) // its note says what it holds, and its memory
		vis.SkipLeave(a)            // killed: no last clean word
		stop()
		b, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		s, ok := deathStory(b, "solo")
		if !ok {
			t.Fatalf("no story of the former process's death: %+v", b.Stories(20, sam))
		}
		t.Logf("%s\n%s", s.Title, s.Body)
		has(t, s.Title, "This member (solo) died and restarted: most likely killed for memory")
		has(t, s.Body, "fleet_test_knob 8")
	})
}

// A value set moments before a kill is in the note: the note is written on
// each change, not only on its tick. In the memory evals an agent's fix landed half
// a second before the kill, and the death story didn't list it as dropped.
func TestAChangeJustBeforeADeathIsInTheNote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		time.Sleep(5 * time.Second) // a note on its tick, without the change
		if _, err := a.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: time.Hour, Reason: "fits in memory"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // well short of the next tick
		vis.SkipLeave(a)
		stop()
		b, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		s, ok := deathStory(b, "solo")
		if !ok {
			t.Fatalf("no story of the former process's death: %+v", b.Stories(20, sam))
		}
		has(t, s.Body, "fleet_test_knob 8")
	})
}

// In a crash loop the second death mustn't bury what the first dropped: in
// the memory evals a person's cache_size 150, which fit, was in the first death's
// story, but the agent was paged after the second, whose story had nothing to
// set back. A dropped value is carried from death to death until someone sets
// the knob again or its lease ends.
func TestACrashLoopKeepsWhatTheFirstDeathDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		if _, err := a.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: sam, For: 24 * time.Hour, Reason: "fits in memory"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		vis.SkipLeave(a)
		stop() // the first death drops 8
		b, stop := alone(t, dir)
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b)
		stop() // the second holds nothing of its own
		c, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		s, ok := deathStory(c, "solo")
		if !ok {
			t.Fatalf("no death story: %+v", c.Stories(20, sam))
		}
		t.Logf("%s\n%s", s.Title, s.Body)
		has(t, s.Body, "restart #2 in the last 10 minutes")
		has(t, s.Body, "fleet_test_knob 8 (set by human:sam")
		has(t, s.Body, `"fleet_test_knob":"8"`)

		// set again, it is no longer carried
		if _, err := c.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: sam, For: time.Hour, Reason: "set back"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		vis.SkipLeave(c)
		stop()
		d, stop2 := alone(t, dir)
		defer stop2()
		time.Sleep(time.Second)
		s, _ = deathStory(d, "solo")
		if n := strings.Count(s.Body, "fleet_test_knob 8"); n != 1 {
			t.Errorf("fleet_test_knob 8 named %d times, want once (held by c, not also carried): %s", n, s.Body)
		}
	})
}

// A restarted process numbers its changes and leases on from the one before,
// so "change 1" means one change, and asking about the former process's says
// what happened to it. In the memory evals an agent made "change 1, lease 1" on
// three processes in a row and couldn't tell which had been lost.
func TestARestartNumbersChangesOnFromTheFormerProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		first, err := a.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: time.Hour, Reason: "fits"})
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		vis.SkipLeave(a)
		stop()
		b, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		next, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: time.Hour, Reason: "set back"})
		if err != nil {
			t.Fatal(err)
		}
		if next.Change <= first.Change || next.Lease <= first.Lease {
			t.Errorf("after the restart: change %d, lease %d; before it: change %d, lease %d: want higher", next.Change, next.Lease, first.Change, first.Lease)
		}
		_, err = b.WaitImpact(context.Background(), first.Change)
		if err == nil {
			t.Fatal("the former process's change measured on this one")
		}
		has(t, err.Error(), "former process")
		has(t, err.Error(), "died")
		_, err = b.Revert(first.Lease, claude, "undo")
		if !errors.Is(err, vis.ErrUnknownLease) {
			t.Fatalf("reverting the former process's lease: %v", err)
		}
		has(t, err.Error(), "former process")
	})
}

// A writer can ask for a value to be kept across a restart: in the memory evals
// each kill undid the agent's fix (a cache that fits), and the next process
// crashed on the default. A kept value is set back by the process that
// replaces it, as its writer, for what is left of its lease, and the death
// story says so and how to undo it.
func TestAKeptValueIsSetBackAfterADeath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		if _, err := a.Apply(vis.Tx{By: claude, For: time.Hour, Keep: true, Reason: "a cache that fits", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(10 * time.Minute)
		vis.SkipLeave(a)
		stop()
		b, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		st, err := b.Stack("fleet_test_knob")
		if err != nil {
			t.Fatal(err)
		}
		if st[0].Value != 8 || st[0].By != claude || !st[0].Kept {
			t.Fatalf("after the restart: %+v, want 8 by %v, kept", st[0], claude)
		}
		if st[0].Left > 50*time.Minute || st[0].Left < 49*time.Minute {
			t.Errorf("lease left %v, want what was left of the hour (about 50m)", st[0].Left)
		}
		s, ok := deathStory(b, "solo")
		if !ok {
			t.Fatalf("no death story: %+v", b.Stories(20, sam))
		}
		t.Logf("%s\n%s", s.Title, s.Body)
		has(t, s.Body, "set back what its writer asked to keep: fleet_test_knob 8 (set by agent:claude, 50m left)")
		has(t, s.Body, fmt.Sprintf("revert lease %d", st[0].Lease))
		if strings.Contains(s.Body, "The restart dropped") {
			t.Errorf("a kept value told as dropped: %s", s.Body)
		}
	})
}

// In a fleet with no state dir, the restarted member hears its kept values
// from its peers, which kept its last digest, and sets them back itself.
func TestAKeptValueIsSetBackThroughTheFleet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.StateDir(""))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		time.Sleep(3 * time.Second)
		if _, err := b.Apply(vis.Tx{By: claude, For: time.Hour, Keep: true, Reason: "a cache that fits", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b)
		stopB()
		time.Sleep(time.Second)
		b2, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		defer func() { stopA(); stop(); synctest.Wait() }()
		time.Sleep(10 * time.Second)
		if st, _ := b2.Stack("fleet_test_knob"); st[0].Value != 8 || !st[0].Kept || st[0].By != claude {
			t.Fatalf("after the restart: %+v", st[0])
		}
		s, _ := deathStory(a, "b")
		has(t, s.Body, "Its writer asked to keep fleet_test_knob 8")
	})
}

// On call for a fleet in a memory crash loop, the dock said c "lost what was
// set on it at runtime", listing as lost the value the breaker had refused to
// set back, with nothing of memory: an operator would set the killer back.
// The fleet says how a member died, what its memory was, and which dropped
// value vis stopped setting back.
func TestAFleetSaysAMemberIsKilledForMemoryAndWhatVisStoppedSettingBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.StateDir(""))
		b, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		time.Sleep(3 * time.Second)
		if _, err := b.Apply(vis.Tx{By: sam, For: 2 * time.Hour, Keep: true, Reason: "more hits", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Second) // it kills the process it is set on
		for range 3 {                // and the fresh one it is set back on, then the crash loop at the default
			vis.SkipLeave(b)
			stop()
			time.Sleep(time.Second)
			b, stop = peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
			time.Sleep(12 * time.Second)
		}
		defer func() { stopA(); stop(); synctest.Wait() }()
		rep := a.Fleet(context.Background())
		var m *vis.FleetMember
		for i := range rep.Members {
			if rep.Members[i].Name == "b" {
				m = &rep.Members[i]
			}
		}
		if m == nil {
			t.Fatalf("no b: %+v", rep.Members)
		}
		if m.Died != "most likely killed for memory" {
			t.Errorf("b died %q", m.Died)
		}
		if m.MemUsed>>20 != 500 || m.MemLimit>>20 != 512 {
			t.Errorf("b's memory: %d of %d MiB", m.MemUsed>>20, m.MemLimit>>20)
		}
		if len(m.Lost) != 1 || !m.Lost[0].Unkept {
			t.Errorf("b's lost: %+v, want fleet_test_knob 8 that vis stopped setting back", m.Lost)
		}
		var why string
		for _, s := range rep.StandOut {
			if s.Member == "b" {
				why = s.Why
			}
		}
		has(t, why, "most likely killed for memory")
		has(t, why, "vis stopped setting back fleet_test_knob 8")
	})
}

// On c's own page, in its crash loop, the headline left out c: the fresh
// process's ring had seen one restart of its name, the peers three. A
// member's view of itself counts the deaths its note keeps.
func TestAMemberInACrashLoopCountsItsOwnDeaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		n := pipenet.New()
		_, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.StateDir(""))
		b, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(dir))
		time.Sleep(5 * time.Second)
		for range 3 {
			vis.SkipLeave(b)
			stop()
			time.Sleep(time.Second)
			b, stop = peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(dir))
			time.Sleep(8 * time.Second)
		}
		defer func() { stopA(); stop(); synctest.Wait() }()
		for _, m := range b.Fleet(context.Background()).Members {
			if m.Self && (m.Restarts != 3 || m.Died != "most likely killed for memory") {
				t.Fatalf("b of itself: %d restarts, died %q; want 3, for memory", m.Restarts, m.Died)
			}
		}
	})
}

// On call, a's TODAY stacked five "died and restarted" stories for two
// members in a crash loop, pushing everything else down. A member's deaths
// within 10 minutes are one story, told from the newest.
func TestACrashLoopIsOneStory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.StateDir(""))
		b, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		time.Sleep(5 * time.Second)
		for range 3 {
			vis.SkipLeave(b)
			stop()
			time.Sleep(time.Second)
			b, stop = peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
			time.Sleep(8 * time.Second)
		}
		defer func() { stopA(); stop(); synctest.Wait() }()
		var deaths []vis.Story
		for _, s := range a.Stories(20, sam) {
			if strings.Contains(s.Title, "died") {
				deaths = append(deaths, s)
			}
		}
		if len(deaths) != 1 {
			t.Fatalf("%d death stories, want 1: %+v", len(deaths), deaths)
		}
		has(t, deaths[0].Title, "b died and restarted 3 times")
		has(t, deaths[0].Body, "restart #3")
	})
}

// A deploy's clean restart keeps a kept value too, with no death to tell.
func TestAKeptValueOutlivesACleanRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		a, stop := alone(t, dir)
		if _, err := a.Apply(vis.Tx{By: claude, For: time.Hour, Keep: true, Reason: "a cache that fits", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		stop()
		b, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		if st, _ := b.Stack("fleet_test_knob"); st[0].Value != 8 || !st[0].Kept {
			t.Fatalf("after a clean restart: %+v", st[0])
		}
	})
}

// A kept value under another change is kept too: the note holds every kept
// lease, not only the value in effect. When the change above it ends, the
// kept value is what the knob falls back to, as before the death.
func TestAKeptValueUnderAnotherChangeIsSetBackAfterADeath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		if _, err := a.Apply(vis.Tx{By: claude, For: time.Hour, Keep: true, Reason: "a cache that fits", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Set(vis.Change{Knob: "fleet_test_knob", Value: 9, By: sam, For: 5 * time.Minute, Reason: "try one more"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(time.Minute)
		vis.SkipLeave(a)
		stop()
		b, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		st, err := b.Stack("fleet_test_knob")
		if err != nil {
			t.Fatal(err)
		}
		if st[0].Value != 8 || st[0].By != claude || !st[0].Kept {
			t.Fatalf("after the restart: %+v, want claude's kept 8", st)
		}
		s, ok := deathStory(b, "solo")
		if !ok {
			t.Fatalf("no death story: %+v", b.Stories(20, sam))
		}
		has(t, s.Body, "The restart dropped what it held: fleet_test_knob 9 (set by human:sam")
		has(t, s.Body, "set back what its writer asked to keep: fleet_test_knob 8 (set by agent:claude")
	})
}

// In a fleet with no note, a clean restart keeps a kept value too: its
// peers keep the last word of a member that left, as they do of one that
// died. A failed liveness probe restarts a pod cleanly, often under the very
// pressure the kept value answers.
func TestAKeptValueOutlivesACleanRestartInAFleet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		a, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.StateDir(""))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		time.Sleep(3 * time.Second)
		first, err := b.Apply(vis.Tx{By: claude, For: time.Hour, Keep: true, Reason: "a cache that fits", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		stopB()
		time.Sleep(time.Second)
		b2, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		defer func() { stopA(); stop(); synctest.Wait() }()
		time.Sleep(10 * time.Second)
		st, _ := b2.Stack("fleet_test_knob")
		if st[0].Value != 8 || !st[0].Kept || st[0].By != claude {
			t.Fatalf("after a clean restart: %+v", st[0])
		}
		if st[0].Lease <= first.Lease {
			t.Errorf("set back under lease %d, at or below the former process's %d: numbering didn't carry on", st[0].Lease, first.Lease)
		}
		if _, ok := deathStory(a, "b"); ok {
			t.Error("a clean restart told as a death")
		}
	})
}

// In a fleet with no note, the restarted member numbers its changes on from
// its former self's, which its peers' last digest of it carries: an ID the
// former process gave out isn't given out again for a different change.
func TestAFleetRestartNumbersChangesOnFromTheFormerProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := pipenet.New()
		_, stopA := peer(t, n, "cacheproxy", "a", "k", "", vis.StateDir(""))
		b, stopB := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		time.Sleep(3 * time.Second)
		first, err := b.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: time.Hour, Reason: "fits"})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		vis.SkipLeave(b)
		stopB()
		time.Sleep(time.Second)
		b2, stop := peer(t, n, "cacheproxy", "b", "k", "a:7946", vis.StateDir(""))
		defer func() { stopA(); stop(); synctest.Wait() }()
		time.Sleep(10 * time.Second)
		next, err := b2.Set(vis.Change{Knob: "fleet_test_knob", Value: 8, By: claude, For: time.Hour, Reason: "set back"})
		if err != nil {
			t.Fatal(err)
		}
		if next.Change <= first.Change || next.Lease <= first.Lease {
			t.Errorf("after the restart: change %d, lease %d; before it: change %d, lease %d: want higher", next.Change, next.Lease, first.Change, first.Lease)
		}
		_, err = b2.WaitImpact(context.Background(), first.Change)
		if err == nil {
			t.Fatal("the former process's change measured on this one")
		}
		has(t, err.Error(), "former process")
	})
}

// A kept value set back after a death gets one chance: if the fresh process
// it was set back on dies again within a minute, it may be what kills it
// (the memory evals' 900 MiB cache in a 512 MiB container died in about 25 s), and
// vis stops setting it back, says why, and how to set it back anyway.
func TestAKeptValueThatKillsIsSetBackOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		if _, err := a.Apply(vis.Tx{By: sam, For: 2 * time.Hour, Keep: true, Reason: "more hits for the sale", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(20 * time.Second) // it kills the process it is set on
		vis.SkipLeave(a)
		stop()
		b, stop := alone(t, dir)
		time.Sleep(time.Second)
		if st, _ := b.Stack("fleet_test_knob"); st[0].Value != 8 {
			t.Fatalf("set by its writer on a running process, it gets its chance: %+v", st[0])
		}
		time.Sleep(25 * time.Second) // and the fresh process it was set back on, soon
		vis.SkipLeave(b)
		stop()
		c, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		if st, _ := c.Stack("fleet_test_knob"); st[0].Value == 8 {
			t.Fatalf("set back again after it killed a fresh process: %+v", st[0])
		}
		s, ok := deathStory(c, "solo")
		if !ok {
			t.Fatalf("no death story: %+v", c.Stories(20, sam))
		}
		t.Logf("%s\n%s", s.Title, s.Body)
		has(t, s.Body, "vis didn't set back fleet_test_knob 8 (set by human:sam")
		has(t, s.Body, "the process it was set back on died about 2")
		has(t, s.Body, "To set it back anyway:")

		time.Sleep(25 * time.Second) // the crash loop goes on, at the default
		vis.SkipLeave(c)
		stop()
		d, stop := alone(t, dir) // stop is the deferred one now
		time.Sleep(time.Second)
		s, _ = deathStory(d, "solo")
		t.Logf("%s\n%s", s.Title, s.Body)
		has(t, s.Body, "which vis stopped setting back, since it may be what kills it")
		has(t, s.Body, "To set it back anyway:") // not offered as the fix
		if strings.Contains(s.Body, `Add "keep":true`) {
			t.Errorf("a later death suggests keeping the value that killed: %s", s.Body)
		}
	})
}

// A kept value that held a fresh process for minutes, as the memory evals' cache
// that fits did while a leak in the code killed it, is set back after every
// death: it didn't kill the process.
func TestAKeptValueThatHeldIsSetBackAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		a, stop := alone(t, dir)
		if _, err := a.Apply(vis.Tx{By: claude, For: time.Hour, Keep: true, Reason: "a cache that fits", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(10 * time.Second)
		vis.SkipLeave(a)
		stop()
		b, stop := alone(t, dir)
		time.Sleep(3 * time.Minute) // the leak kills it, minutes on
		vis.SkipLeave(b)
		stop()
		c, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		if st, _ := c.Stack("fleet_test_knob"); st[0].Value != 8 || !st[0].Kept {
			t.Fatalf("a kept value that held, after the next death: %+v", st[0])
		}
	})
}

// Kept with no lease would outlive every restart, the fix and its reason
// long gone: keep needs a lease.
func TestKeepNeedsALease(t *testing.T) {
	in := vis.NewInstance()
	_, err := in.Apply(vis.Tx{By: sam, Keep: true, Reason: "forever", Set: []vis.Write{{Knob: "fleet_test_knob", Value: 8}}})
	if err == nil {
		t.Fatal("kept with no lease")
	}
	has(t, err.Error(), "keep needs a lease")
}

// A stop through Stop, as the guide's main does on SIGTERM, leaves a clean
// note: the next process tells no death.
func TestACleanStopAloneIsNoDeath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		_, stop := alone(t, dir)
		time.Sleep(3 * time.Second)
		stop()
		b, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		if s, ok := deathStory(b, ""); ok {
			t.Fatalf("a clean stop told as a death: %+v", s)
		}
	})
}

// The note is optional: a directory that can't hold it (read-only, or not a
// directory) doesn't stop the instance; it is a warning, saying what is lost
// and how to fix it.
func TestANoteThatCantBeWrittenIsAWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notADir := filepath.Join(t.TempDir(), "file")
		os.WriteFile(notADir, nil, 0o644)
		in, stop := alone(t, notADir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		w := strings.Join(in.Warnings(), "\n")
		has(t, w, "can't keep its last word in "+notADir)
		has(t, w, "VIS_STATE_DIR")
	})
}

// fakeNote writes a note as process pid would have, one that died holding a
// kept value: what a process alone reads when it starts.
func fakeNote(t *testing.T, dir string, pid int) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("vis-cacheproxy-solo-%d.json", pid))
	note := fmt.Sprintf(`{"id":"other","name":"solo","health":{"at":%d,"cpu_used":0,"cpu_limit":0,"throttled":0,"slos":null,"changes":5,"leases":5,`+
		`"held":[{"knob":"fleet_test_knob","value":"8","by":"agent:claude","ends_in":3600000000000}],`+
		`"kept":[{"knob":"fleet_test_knob","value":"8","by":"agent:claude","ends_in":3600000000000}]}}`, time.Now().UnixNano())
	if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Two processes of a service on one host (two systemd units, go run twice, a
// restart that overlaps) share a name. Each keeps its own note, by PID, and a
// note whose process is alive isn't a former self's: no death is told, its
// kept values stay its own, and vis warns that two processes share a name.
func TestALiveProcesssNoteIsNoFormerSelfs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		other := fakeNote(t, dir, os.Getppid()) // alive: it runs this test
		in, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		if s, ok := deathStory(in, ""); ok {
			t.Errorf("a live process's note told as a death: %s %s", s.Title, s.Body)
		}
		if st, _ := in.Stack("fleet_test_knob"); st[0].Value == 8 {
			t.Errorf("took a live process's kept value: %+v", st[0])
		}
		if _, err := os.Stat(other); err != nil {
			t.Errorf("the live process's note: %v", err)
		}
		w := strings.Join(in.Warnings(), "\n")
		has(t, w, fmt.Sprintf("process %d", os.Getppid()))
		has(t, w, "VIS_INSTANCE_NAME")
	})
}

// A note whose process is gone is a former self's, whatever its PID: it is
// read, taken over from, and removed, so notes don't pile up on a host.
func TestADeadProcesssNoteIsTakenOverAndRemoved(t *testing.T) {
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Skip("no true to run:", err)
	}
	synctest.Test(t, func(t *testing.T) {
		nearItsLimit(t)
		dir := t.TempDir()
		path := fakeNote(t, dir, gone.Process.Pid)
		in, stop := alone(t, dir)
		defer func() { stop(); synctest.Wait() }()
		time.Sleep(time.Second)
		if _, ok := deathStory(in, "solo"); !ok {
			t.Fatalf("no death story: %+v", in.Stories(20, sam))
		}
		if st, _ := in.Stack("fleet_test_knob"); st[0].Value != 8 || !st[0].Kept {
			t.Errorf("the kept value: %+v", st[0])
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the dead process's note is still there: %v", err)
		}
	})
}

// Where the container's memory isn't what the heap keeps, the heap profile
// says so, and a limit the Go runtime doesn't know is a warning.
func TestAHeapProfileInAContainerSaysWhatItLeavesOut(t *testing.T) {
	nearItsLimit(t)
	in := vis.NewInstance(vis.Service("cacheproxy"))
	r, err := in.Profile(context.Background(), "heap", 0)
	if err != nil {
		t.Fatal(err)
	}
	has(t, r.Summary, "The container uses 500 of its 512 MiB limit")
	if os.Getenv("GOMEMLIMIT") == "" {
		has(t, strings.Join(in.Warnings(), "\n"), "GOMEMLIMIT isn't set")
	}
}
