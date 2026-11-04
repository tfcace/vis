package vis_test

import (
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/tfcace/vis"
)

var (
	sam     = vis.Writer{Kind: vis.Human, ID: "sam"}
	alex    = vis.Writer{Kind: vis.Human, ID: "alex"}
	claude  = vis.Writer{Kind: vis.Agent, ID: "claude"}
	sched   = vis.Writer{Kind: vis.Rule, ID: "sched"}
	rollout = vis.Writer{Kind: vis.Fleet, ID: "rollout"}
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler)) // tests that check the audit trail pass their own logger
	dir, err := os.MkdirTemp("", "vis-test-state")
	if err != nil {
		panic(err)
	}
	os.Setenv("VIS_STATE_DIR", dir) // vis.Start keeps its last word here: never a note an earlier run left
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func uniq(prefix string) string { return fmt.Sprintf("%s_%d", prefix, lateN.Add(1)) }

func signalNamed(in *vis.Instance, name string) (vis.SignalValue, bool) {
	for _, s := range in.Signals() {
		if s.Name == name {
			return s, true
		}
	}
	return vis.SignalValue{}, false
}

func panicOf(f func()) (p string) {
	defer func() {
		if r := recover(); r != nil {
			p = fmt.Sprint(r)
		}
	}()
	f()
	return ""
}
