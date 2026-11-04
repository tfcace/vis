package vis_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/tfcace/vis"
)

var logKnob = vis.NewInt("logger_test_knob", vis.Range(1, 9), vis.Default(5))

// An app that installs its logger after the instance exists gets vis's
// records in that logger, once, as records of their own.
func TestLogsGoToTheDefaultLoggerInEffectNow(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	in := vis.NewInstance() // before the app sets its logger
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	if _, err := in.Set(vis.Change{Knob: "logger_test_knob", Value: 7, By: vis.Writer{Kind: vis.Human, ID: "sam"}}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not one JSON record: %q", line)
		}
		msg, _ := rec["msg"].(string)
		if strings.HasPrefix(msg, "INFO ") || strings.Contains(msg, "knob=") {
			t.Fatalf("a vis record was wrapped in another: %q", line)
		}
		if rec["knob"] == "logger_test_knob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no vis record for the change in the app's logger:\n%s", buf.String())
	}
}

// WithLogger still wins over the default.
func TestWithLoggerKeepsItsLogger(t *testing.T) {
	var mine, def bytes.Buffer
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&def, nil)))
	in := vis.NewInstance(vis.WithLogger(slog.New(slog.NewJSONHandler(&mine, nil))))
	if _, err := in.Set(vis.Change{Knob: "logger_test_knob", Value: 6, By: vis.Writer{Kind: vis.Human, ID: "sam"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mine.String(), "logger_test_knob") || def.Len() != 0 {
		t.Fatalf("WithLogger's logger got %q, the default got %q", mine.String(), def.String())
	}
}
