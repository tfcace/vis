package vis_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tfcace/vis"
)

// vis.Start is the whole adoption in main: the default instance takes the
// options, serves the UI and MCP, and package-level calls reach it.
// Start runs once per process, so this is the one test that calls it.
func TestStartServesTheDefaultInstance(t *testing.T) {
	var mu sync.Mutex
	var built []int
	batchSize.Bind(func(n int) { mu.Lock(); built = append(built, n); mu.Unlock() }) // before Start: state built from a knob
	var logs lockedBuffer                                                            // declared rules log from their own goroutines
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if err := vis.Start(ctx, vis.MCPAt(taken.Addr().String()), vis.UIAt("")); err == nil || errors.Is(err, vis.ErrStarted) {
		t.Fatalf("Start on a taken port = %v, want that error", err)
	} // nothing is serving, so Start may be tried again:
	err = vis.Start(ctx, vis.Service("started"), vis.SLO("GET /k", 50*time.Millisecond),
		vis.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))), vis.MCPAt(""), vis.UIAt("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	ui := regexp.MustCompile(`vis: control UI" url=(\S+)`).FindStringSubmatch(logs.String())
	if ui == nil {
		t.Fatalf("no UI address in the log: %s", logs.String())
	}
	res, err := http.Get(ui[1] + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `"service":"started"`) || !strings.Contains(string(body), `"route":"GET /k"`) {
		t.Fatalf("state = %.300s", body)
	}

	if _, err := vis.DefaultInstance().Apply(vis.Tx{By: sam, Reason: "test", Set: []vis.Write{{Knob: batchSize.Name(), Value: 32}}}); err != nil {
		t.Fatal(err)
	}
	if got := batchSize.Now(); got != 32 {
		t.Fatalf("Now = %d, want 32", got)
	}
	mu.Lock()
	saw := slices.Clone(built)
	mu.Unlock()
	if len(saw) < 2 || saw[0] != 64 || saw[len(saw)-1] != 32 {
		t.Fatalf("Bind saw %v, want 64 first and 32 last", saw)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /k", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", batchSize.From(r.Context())))
	})
	if rec := serve(vis.HTTP(mux), "GET", "/k"); rec.Body.Len() != 32 {
		t.Fatalf("a request through vis.HTTP read %d, want 32", rec.Body.Len())
	}
	if r := vis.DefaultInstance().Route("GET /k", time.Minute); r.Requests != 1 {
		t.Fatalf("route = %+v, want the request timed on the default instance", r)
	}
	if err := vis.Start(ctx); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("a second Start = %v, want an error saying it already ran", err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// VIS_UI_LISTEN sets the UI's address, as VIS_LISTEN does MCP's; options in code win.
func TestTheUIAddressComesFromTheEnvironment(t *testing.T) {
	t.Setenv("VIS_UI_LISTEN", "127.0.0.1:0")
	in := vis.NewInstance()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := in.Serve(ctx, vis.MCPAt(""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(s.UI, ":7071") || s.UI == "" {
		t.Fatalf("UI at %q, want the address from VIS_UI_LISTEN", s.UI)
	}
}
