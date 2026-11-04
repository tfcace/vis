package vis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// An instance alone has no peers to keep its last word when it dies, so it
// keeps it in a file: the digest it would gossip (its memory, what it holds),
// rewritten every noteEvery, and marked clean when it stops through Stop. The
// process that replaces it reads the note before writing its own, and tells
// the death as a fleet does (death.go). It is written again on each change, so a
// value set just before a kill is in it. Docker keeps the file across a
// restart; Kubernetes keeps it in an emptyDir volume. In a fleet the peers
// keep the same word, and each death is told once whichever tells it first.

const noteEvery = 2 * time.Second

// StateDir keeps the instance's last word in dir, so the process that replaces
// it can say why it died: VIS_STATE_DIR, or for Start the temp dir. Give it a
// directory that outlives the process: an emptyDir volume on Kubernetes.
func StateDir(dir string) ServeOption { return func(o *serveOptions) { o.stateDir = dir } }

// lastWord is what the note holds.
type lastWord struct {
	ID     string      `json:"id"` // the process that wrote it
	Name   string      `json:"name"`
	Clean  bool        `json:"clean,omitempty"` // it stopped through Stop: no death to tell
	Health *wireHealth `json:"health,omitempty"`
	Deaths []int64     `json:"deaths,omitempty"` // when earlier processes died, Unix nanoseconds, the last 10 minutes
}

// formerProcess is what a process knows of the one it replaced.
type formerProcess struct {
	changes, leases uint64 // its last IDs: the ones at or below are its
	at              time.Time
	died            bool
	forMemory       bool          // it died, most likely killed for memory (killedForMemory)
	up              time.Duration // how long it ran, when its last word says (wireHealth.Started)
}

// formerOf says, for an ID this process doesn't know, whether it was the
// former process's, and what became of it; nil if not.
func (in *Instance) formerOf(what string, id uint64) error {
	f := in.former.Load()
	if f == nil || id == 0 || what == "change" && id > f.changes || what == "lease" && id > f.leases {
		return nil
	}
	how := "stopped"
	if f.died {
		how = "died"
	}
	return fmt.Errorf("%s %d was the former process's: it %s at %s, and a restart drops its changes, its leases and their measurements; this process has its own since",
		what, id, how, f.at.UTC().Format("15:04:05 MST"))
}

var noteName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// notePath is this process's note: by service, name and PID, so two
// processes of a service on one host (two systemd units, go run twice, a
// restart that overlaps) each keep their own.
func (in *Instance) notePath(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("%s%d.json", in.notePrefix(), os.Getpid()))
}

func (in *Instance) notePrefix() string {
	return "vis-" + noteName.ReplaceAllString(cmpOrStr(in.service, "service")+"-"+in.name, "_") + "-"
}

func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// readNote reads the former process's note and takes over from it
// (succeed). It returns the recent deaths, this one's included. Of the notes
// under this service and name, one whose process is alive is another
// process's (a warning); of the rest, the newest is the former process's, and
// it is removed once taken over from. In a container the process that
// replaces a dead one often has its PID, and so its note's path.
func (in *Instance) readNote(path string) (deaths []int64) {
	dir, prefix := filepath.Dir(path), in.notePrefix()
	entries, _ := os.ReadDir(dir)
	var w lastWord
	var from string
	for _, e := range entries {
		digits, ok := strings.CutSuffix(strings.TrimPrefix(e.Name(), prefix), ".json")
		pid, err := strconv.Atoi(digits)
		if !ok || !strings.HasPrefix(e.Name(), prefix) || err != nil || pid <= 0 {
			continue
		}
		if pid != os.Getpid() && processAlive(pid) {
			in.noteTwin.Store(int64(pid))
			continue
		}
		var n lastWord
		p := filepath.Join(dir, e.Name())
		if b, err := os.ReadFile(p); err != nil || json.Unmarshal(b, &n) != nil || n.ID == "" || n.ID == in.id || n.Health == nil {
			continue
		}
		if w.Health == nil || n.Health.At > w.Health.At {
			w, from = n, p
		}
	}
	if w.Health == nil {
		return nil
	}
	deaths = recentDeaths(w.Deaths)
	at := time.Unix(0, w.Health.At)
	if !w.Clean {
		deaths = append(deaths, at.UnixNano())
	}
	in.deathMu.Lock()
	in.ownDeaths = deaths
	in.deathMu.Unlock()
	in.succeed(cmpOrStr(w.Name, in.name), restart{id: w.ID, last: w.Health, died: at, clean: w.Clean}, len(deaths))
	if from != path {
		os.Remove(from) // taken over: this process's note carries on what it told
	}
	return deaths
}

// succeed takes over from the process this one replaced, once, whichever word
// of it comes first: its note, or its peers' last digest of it (a death) or
// last word (a clean leave). It numbers this process's changes and leases on
// from that one's, sets back what its writers asked to keep, and if it died,
// tells its death. A word that comes through the fleet may come after this
// process made changes of its own; numbering then carries on from there.
func (in *Instance) succeed(name string, r restart, n int) {
	in.deathMu.Lock()
	if in.succeeded == nil {
		in.succeeded = map[string]bool{}
	}
	done := in.succeeded[r.id]
	in.succeeded[r.id] = true
	in.deathMu.Unlock()
	if done || r.last == nil {
		return
	}
	h := r.last
	in.mu.Lock()
	in.change, in.leaseN = max(in.change, h.Changes), max(in.leaseN, h.Leases)
	in.mu.Unlock()
	f := &formerProcess{changes: h.Changes, leases: h.Leases, at: time.Unix(0, h.At), died: !r.clean, forMemory: !r.clean && killedForMemory(h.Mem), up: time.Hour}
	if h.Started != 0 {
		f.up = time.Unix(0, h.At).Sub(time.Unix(0, h.Started))
	}
	in.former.Store(f)
	back := map[string]uint64{} // kept values set back, by heldKey: their new lease, 0 if it failed
	gone := time.Since(time.Unix(0, h.At))
	for _, k := range h.Kept {
		if !r.clean && killedItsProcess(k) { // it may be what killed it: told, not set back
			continue
		}
		if k, ok := leaseLeft(k, gone); ok {
			lease, err := in.setBackKept(k)
			if err != nil {
				in.logger().Warn("vis: can't set back a kept value after the restart", "knob", k.Knob, "value", k.Value, "err", err)
			}
			back[heldKey(k)] = lease
		}
	}
	if !r.clean && time.Since(r.died) < 24*time.Hour { // older is news nobody acts on
		in.noteDeath(name, r, n, back)
	}
}

// keepNote keeps this process's note until ctx ends, marks it clean unless
// the instance is told to skip its leaving (as a kill would), waits for the
// rest of serving to stop (inner), and only then closes done, so Stop returns
// after the clean mark.
func (in *Instance) keepNote(ctx context.Context, path string, deaths []int64, inner, done chan struct{}) {
	defer close(done)
	write := func(clean bool) {
		w := lastWord{ID: in.id, Name: in.name, Clean: clean, Health: in.health(), Deaths: recentDeaths(deaths)}
		b, err := json.Marshal(w)
		if err == nil {
			tmp := path + ".tmp"
			if err = os.WriteFile(tmp, b, 0o600); err == nil {
				err = os.Rename(tmp, path) // whole or not at all: a kill mid-write leaves the last note
			}
		}
		in.noteFailed(filepath.Dir(path), err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700) // a scratch image has no /tmp; if it can't, the write says why
	write(false)
	t := time.NewTicker(noteEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			write(false)
		case <-in.changed(): // a value set just before a kill is the one most worth naming
			write(false)
		case <-ctx.Done():
			if !in.skipLeave.Load() {
				write(true)
			}
			<-inner
			return
		}
	}
}

// noteFailed keeps why the note can't be written, for Warnings, logging it
// once when it starts failing; nil clears it. The note is optional: the
// instance goes on without it.
func (in *Instance) noteFailed(dir string, err error) {
	if err == nil {
		in.noteErr.Store(nil)
		return
	}
	why := err.Error()
	if prev := in.noteErr.Swap(&why); prev == nil {
		in.logger().Warn("vis: can't keep its last word, so a death won't be explained after a restart", "dir", dir, "err", err)
	}
	in.noteDir.Store(&dir)
}

// twinWarning is the warning for another live process of this service under
// this name on this host, alone: in a fleet, members sharing a name are
// warned of already.
func (in *Instance) twinWarning() string {
	pid := int(in.noteTwin.Load())
	if pid == 0 || in.ring.Load() != nil || !processAlive(pid) {
		return ""
	}
	return fmt.Sprintf("Another process (process %d) runs on this host as %s of %s. Each keeps its own last word, but a person or agent reading either can't tell them apart. Give each its own VIS_INSTANCE_NAME.",
		pid, in.name, cmpOrStr(in.service, "this service"))
}

// noteWarning is the note's failure as a warning, when it matters: alone. In
// a fleet the peers keep the last word.
func (in *Instance) noteWarning() string {
	why, dir := in.noteErr.Load(), in.noteDir.Load()
	if why == nil || dir == nil || in.ring.Load() != nil {
		return ""
	}
	return fmt.Sprintf("vis can't keep its last word in %s (%s), so if this process dies without stopping cleanly, the one that replaces it can't say why or what it dropped. Set VIS_STATE_DIR to a writable directory that outlives the process (an emptyDir volume on Kubernetes).", *dir, *why)
}

// recentDeaths keeps the deaths of the last 10 minutes, for "restart #3".
func recentDeaths(ds []int64) []int64 {
	cut := time.Now().Add(-keepDead).UnixNano()
	return slices.DeleteFunc(slices.Clone(ds), func(d int64) bool { return d < cut })
}
