package vis_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

func info(t *testing.T, name string) vis.Info {
	t.Helper()
	for _, k := range vis.Knobs() {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("no knob %s", name)
	return vis.Info{}
}

// The environment sets a knob's default, so a service configured that way
// keeps its variables working.
func TestEnvSetsAKnobsDefault(t *testing.T) {
	t.Setenv("VIS_T_LOADED", "3")
	k := vis.NewInt("env_t_loaded", vis.Range(0, 8), vis.Default(0), vis.Env("VIS_T_LOADED"))
	if got := vis.NewInstance().Snapshot(); k.Get(got) != 3 {
		t.Fatalf("value = %d, want 3 from VIS_T_LOADED", k.Get(got))
	}
	if i := info(t, "env_t_loaded"); i.Default != 3 || i.Env != "VIS_T_LOADED" || !i.FromEnv {
		t.Fatalf("info = %+v, want default 3 from VIS_T_LOADED", i)
	}

	t.Setenv("VIS_T_WAIT", "250ms")
	d := vis.NewDuration("env_t_wait", vis.Range(time.Millisecond, time.Second), vis.Default(100*time.Millisecond), vis.Env("VIS_T_WAIT"))
	if got := d.Get(vis.NewInstance().Snapshot()); got != 250*time.Millisecond {
		t.Fatalf("duration = %v, want 250ms from VIS_T_WAIT", got)
	}
}

// Unset or empty, the code's default stands.
func TestEnvUnsetKeepsTheCodesDefault(t *testing.T) {
	t.Setenv("VIS_T_EMPTY", "")
	k := vis.NewInt("env_t_empty", vis.Range(0, 8), vis.Default(2), vis.Env("VIS_T_EMPTY"))
	u := vis.NewInt("env_t_unset", vis.Range(0, 8), vis.Default(2), vis.Env("VIS_T_NEVER_SET"))
	snap := vis.NewInstance().Snapshot()
	if k.Get(snap) != 2 || u.Get(snap) != 2 || info(t, "env_t_empty").FromEnv {
		t.Fatalf("values = %d, %d, want the code's 2", k.Get(snap), u.Get(snap))
	}
}

// A bad value is deployment input, not code: the code's default stands, and
// vis says why, where people and agents look.
func TestEnvBadValuesKeepTheDefaultAndWarn(t *testing.T) {
	t.Setenv("VIS_T_WORD", "lots")
	t.Setenv("VIS_T_BIG", "99")
	t.Setenv("VIS_T_BARE", "300")
	w := vis.NewInt("env_t_word", vis.Range(0, 8), vis.Default(1), vis.Env("VIS_T_WORD"))
	b := vis.NewInt("env_t_big", vis.Range(0, 8), vis.Default(1), vis.Env("VIS_T_BIG"))
	d := vis.NewDuration("env_t_bare", vis.Range(time.Second, time.Hour), vis.Default(time.Minute), vis.Env("VIS_T_BARE"))
	in := vis.NewInstance()
	snap := in.Snapshot()
	if w.Get(snap) != 1 || b.Get(snap) != 1 || d.Get(snap) != time.Minute {
		t.Fatalf("values = %d, %d, %v; want the code's defaults", w.Get(snap), b.Get(snap), d.Get(snap))
	}
	all := strings.Join(in.Warnings(), "\n")
	for _, want := range []string{
		`VIS_T_WORD="lots" isn't a whole number, so env_t_word keeps its default 1`,
		`VIS_T_BIG="99" is outside env_t_big's range 0..8, so env_t_big keeps its default 1`,
		`VIS_T_BARE="300" isn't a duration such as 300s, so env_t_bare keeps its default 1m0s`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings lack %q:\n%s", want, all)
		}
	}
}

// An operator who set the variable reads the process's log, not the UI: a
// value vis didn't take is logged when the instance starts serving.
func TestEnvBadValueIsLoggedWhenServing(t *testing.T) {
	t.Setenv("VIS_T_ZERO", "0")
	vis.NewInt("env_t_zero", vis.Range(1, 8), vis.Default(3), vis.Env("VIS_T_ZERO"))
	var logs bytes.Buffer
	in := vis.NewInstance(vis.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := in.Serve(ctx, vis.MCPAt(""), vis.UIAt("")); err != nil {
		t.Fatal(err)
	}
	if want := `VIS_T_ZERO=\"0\" is outside env_t_zero's range 1..8, so env_t_zero keeps its default 3`; !strings.Contains(logs.String(), want) || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("log lacks a warning %q:\n%s", want, logs.String())
	}
}
