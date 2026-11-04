package vis_test

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tfcace/vis"
)

func only(t *testing.T, s []vis.Story, want int) []vis.Story {
	t.Helper()
	if len(s) != want {
		var titles []string
		for _, x := range s {
			titles = append(titles, x.Title)
		}
		t.Fatalf("got %d stories, want %d: %q", len(s), want, titles)
	}
	return s
}

func has(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%q\ndoes not contain\n%q", got, want)
	}
}

// One story follows a rule's whole run, and stays true as it unfolds.
func TestARuleRunIsOneStoryThatStaysTrue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(75)
		r := batchRule(s)
		r.Lease = time.Minute
		runRule(t, in, r)
		time.Sleep(25 * time.Second) // two steps, then the stall
		synctest.Wait()
		st := only(t, in.Stories(10, sam), 1)[0]
		if st.Title != "Rule batch_backoff moved test_batch_size from 64 to 32, then stopped: its steps didn't help." || !st.Alert || st.Kind != "rule" {
			t.Fatalf("while stalled: %+v", st)
		}
		has(t, st.Body, "sched.p99 stayed at 75 ms against its 5 ms limit.")
		has(t, st.Body, "It holds test_batch_size at 32 for another")
		time.Sleep(time.Minute) // its lease runs out
		synctest.Wait()
		st = only(t, in.Stories(10, sam), 1)[0]
		has(t, st.Body, "Its change ran out")
		has(t, st.Body, "test_batch_size is back to 64.")
		if strings.Contains(st.Body, "It holds") {
			t.Fatalf("the story still says the rule holds after its lease ended: %q", st.Body)
		}
	})
}

// A deliberate change, its warning and its end are one story; values carry their units.
func TestAChangeStoryWarnsAndThenEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: 30 * time.Minute, By: claude, Reason: "hit rate is 71%"})
		st := only(t, in.Stories(10, sam), 1)[0]
		if st.Title != "Agent claude set test_cache_size to 2000 MiB." || st.Alert || st.Lease == 0 {
			t.Fatalf("new change: %+v", st)
		}
		has(t, st.Body, "“hit rate is 71%”")
		has(t, st.Body, "It was 1000 MiB.")
		has(t, st.Body, "then back to 1000 MiB.")
		time.Sleep(27 * time.Minute) // the warning, a tenth of the lease ahead
		synctest.Wait()
		st = only(t, in.Stories(10, sam), 1)[0]
		if !st.Alert {
			t.Fatalf("an ending change should ask for attention: %+v", st)
		}
		has(t, st.Body, "Ends in 3m, then back to 1000 MiB.")
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		st = only(t, in.Stories(10, sam), 1)[0]
		if st.Alert {
			t.Fatalf("an ended change still asks for attention: %+v", st)
		}
		has(t, st.Body, "It ended after 30m: test_cache_size is back to 1000 MiB.")
	})
}

// A story's title also comes in parts, so a page can set who acted, the rule,
// the knob and its values apart from the words around them. The parts say
// exactly the title.
func TestStoryTitlesComeInParts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: 30 * time.Minute, By: claude, Reason: "hit rate is 71%"})
		st := in.Stories(10, sam)[0]
		if got := partsOf(t, st); got != "Agent [actor:claude] set [knob:test_cache_size] to [value:2000 MiB]." {
			t.Fatalf("parts = %s", got)
		}
		time.Sleep(time.Minute) // so it's the newer story
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam})
		if got := partsOf(t, in.Stories(10, sam)[0]); got != "[actor:You] pinned [knob:test_cache_size] to [value:3000 MiB]." {
			t.Fatalf("as sam: %s", got)
		}
		if got := partsOf(t, in.Stories(10, alex)[0]); got != "[actor:Sam] pinned [knob:test_cache_size] to [value:3000 MiB]." {
			t.Fatalf("as alex: %s", got)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		in := vis.NewInstance()
		s := &signal{}
		s.set(75)
		r := batchRule(s)
		r.Lease = time.Minute
		runRule(t, in, r)
		time.Sleep(25 * time.Second)
		synctest.Wait()
		if got := partsOf(t, in.Stories(10, sam)[0]); got != "Rule [rule:batch_backoff] moved [knob:test_batch_size] from [value:64] to [value:32], then stopped: its steps didn't help." {
			t.Fatalf("rule: %s", got)
		}
	})
}

// partsOf writes a title's parts with their kinds marked, after checking they
// say the title.
func partsOf(t *testing.T, st vis.Story) string {
	t.Helper()
	var plain, marked strings.Builder
	for _, p := range st.TitleParts {
		plain.WriteString(p.Text)
		if p.Kind == "" {
			marked.WriteString(p.Text)
		} else {
			marked.WriteString("[" + p.Kind + ":" + p.Text + "]")
		}
	}
	if plain.String() != st.Title {
		t.Fatalf("parts say %q, the title is %q", plain.String(), st.Title)
	}
	return marked.String()
}

func TestStoriesSayYouToTheirViewer(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam})
	if st := in.Stories(10, sam)[0]; st.Title != "You pinned test_cache_size to 3000 MiB." {
		t.Fatalf("as sam: %q", st.Title)
	}
	if st := in.Stories(10, alex)[0]; st.Title != "Sam pinned test_cache_size to 3000 MiB." {
		t.Fatalf("as alex: %q", st.Title)
	}
}

func TestAFreezeAndItsUnfreezeAreOneStory(t *testing.T) {
	in := vis.NewInstance()
	mustFreeze(t, in, sam, "investigating")
	st := only(t, in.Stories(10, alex), 1)[0]
	if st.Title != "Sam froze automated changes." || !st.Alert {
		t.Fatalf("frozen: %+v", st)
	}
	has(t, st.Body, "“investigating”")
	if st.Why != "investigating" {
		t.Fatalf("why = %q, want the freeze's reason", st.Why)
	}
	if err := in.Unfreeze(alex, "spike over"); err != nil {
		t.Fatal(err)
	}
	st = only(t, in.Stories(10, alex), 1)[0]
	if st.Alert || st.Title != "Sam froze automated changes; you unfroze them." {
		t.Fatalf("unfrozen: %+v", st)
	}
	has(t, st.Body, "“spike over”")
}

func TestStoriesComeNewestFirst(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 3000, By: sam})
	mustSet(t, in, vis.Change{Knob: "test_batch_size", Value: 100, By: sam})
	s := in.Stories(10, sam)
	if len(s) != 2 || s[0].Knob != "test_batch_size" || s[1].Knob != "test_cache_size" {
		t.Fatalf("stories = %+v", s)
	}
	if len(in.Stories(1, sam)) != 1 {
		t.Fatal("Stories(1) returned more than one")
	}
}

func TestStoryDurationsReadNaturally(t *testing.T) {
	in := vis.NewInstance()
	for _, d := range []time.Duration{30 * time.Minute, 90 * time.Second, 49 * time.Second, 65 * time.Minute} {
		mustSet(t, in, vis.Change{Knob: "test_cache_size", Value: 2000, For: d, By: claude})
	}
	var got []string
	for _, s := range in.Stories(10, sam) {
		got = append(got, s.Body[strings.Index(s.Body, "For "):])
	}
	want := []string{"For 1h5m, then back to 1000 MiB.", "For 49s, then back to 1000 MiB.", "For 1m30s, then back to 1000 MiB.", "For 30m, then back to 1000 MiB."}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

var batchItems = vis.NewInt("test_batch_items", vis.Range(1, 512), vis.Default(64), vis.Unit("items"), vis.Pull(vis.Agent, vis.Rule))

// One of a counted unit reads in the singular; units that aren't counted
// nouns ("ms", "rps", "keys/s") are left alone.
func TestStoriesSayOneItemNotOneItems(t *testing.T) {
	in := vis.NewInstance()
	mustSet(t, in, vis.Change{Knob: "test_batch_items", Value: 1, By: sam})
	if st := in.Stories(10, alex)[0]; st.Title != "Sam pinned test_batch_items to 1 item." {
		t.Fatalf("title = %q", st.Title)
	}
	mustSet(t, in, vis.Change{Knob: "test_batch_items", Value: 2, By: sam})
	if st := in.Stories(10, alex)[0]; st.Title != "Sam pinned test_batch_items to 2 items." {
		t.Fatalf("title = %q", st.Title)
	}
}
