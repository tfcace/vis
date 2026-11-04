package vis_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/tfcace/vis"
)

// A taken port fails at startup saying which setting moves it.
func TestServeSaysWhichSettingMovesATakenPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := vis.NewInstance().Serve(ctx, vis.MCPAt("127.0.0.1:0"), vis.UIAt("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = vis.NewInstance().Serve(ctx, vis.MCPAt(strings.TrimPrefix(first.MCP, "http://")), vis.UIAt(""))
	if err == nil || !strings.Contains(err.Error(), "VIS_LISTEN") || !strings.Contains(err.Error(), "vis.MCPAt") {
		t.Fatalf("MCP's port taken: %v, want it to name VIS_LISTEN and vis.MCPAt", err)
	}
	_, err = vis.NewInstance().Serve(ctx, vis.MCPAt(""), vis.UIAt(strings.TrimPrefix(first.UI, "http://")))
	if err == nil || !strings.Contains(err.Error(), "VIS_UI_LISTEN") {
		t.Fatalf("the UI's port taken: %v, want it to name VIS_UI_LISTEN", err)
	}
}

func TestServeStartsMCPAndTheUIInOneCall(t *testing.T) {
	var logs bytes.Buffer
	in := vis.NewInstance(vis.Service("demo"), vis.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Setenv("VIS_TOKENS", "s3cret=agent:oncall:act")
	t.Setenv("VIS_UI_USER", "sam")
	s, err := in.Serve(ctx, vis.MCPAt("127.0.0.1:0"), vis.UIAt("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(s.UI + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `"user":"human:sam"`) || !strings.Contains(string(body), `"service":"demo"`) {
		t.Fatalf("UI state = %s", body)
	}
	cs := connectURL(t, s.MCP, "s3cret") // VIS_TOKENS was honored: this token can act
	if out, isErr := call(t, cs, "freeze", map[string]any{"reason": "from a test"}); isErr {
		t.Fatal(out)
	}
	for _, want := range []string{s.MCP, s.UI, "act", "human:sam"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("startup log doesn't mention %q:\n%s", want, logs.String())
		}
	}
}

func TestServeFailsAtStartupOnABadAddress(t *testing.T) {
	in := vis.NewInstance()
	if _, err := in.Serve(context.Background(), vis.MCPAt("256.0.0.1:1"), vis.UIAt("")); err == nil {
		t.Fatal("served on an impossible address")
	}
}

func TestServeStopsWithItsContext(t *testing.T) {
	in := vis.NewInstance()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := in.Serve(ctx, vis.MCPAt(""), vis.UIAt("127.0.0.1:0"))
	if err != nil || s.MCP != "" {
		t.Fatalf("served %+v, %v; want the UI only", s, err)
	}
	cancel()
	for i := 0; ; i++ { // shutdown is asynchronous; it must happen soon
		if _, err := http.Get(s.UI + "/api/state"); err != nil {
			break
		}
		if i > 200 {
			t.Fatal("still serving after its context ended")
		}
	}
}

// VIS_LISTEN="" turns MCP off, as the guide says.
func TestAnEmptyListenTurnsMCPOff(t *testing.T) {
	t.Setenv("VIS_LISTEN", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := vis.NewInstance().Serve(ctx, vis.UIAt("127.0.0.1:0"))
	if err != nil || s.MCP != "" {
		t.Fatalf("served %+v, %v; want MCP off", s, err)
	}
}

// unsetenv clears key for the test, restoring it after.
func unsetenv(t *testing.T, key string) {
	t.Helper()
	if v, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() { os.Setenv(key, v) })
	}
	os.Unsetenv(key)
}

// A default port another service holds doesn't stop this one: vis serves on
// a free port and says where, and how to choose one.
func TestServeMovesOffATakenDefaultPort(t *testing.T) {
	unsetenv(t, "VIS_LISTEN")
	unsetenv(t, "VIS_UI_LISTEN")
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	takenUI, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer takenUI.Close()
	defer vis.SetDefaultAddrs(taken.Addr().String(), takenUI.Addr().String())()
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := vis.NewInstance(vis.WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))).Serve(ctx)
	if err != nil {
		t.Fatalf("Serve with its default ports taken = %v, want it to serve elsewhere", err)
	}
	for _, c := range []struct{ url, taken, env string }{{s.MCP, taken.Addr().String(), "VIS_LISTEN"}, {s.UI, takenUI.Addr().String(), "VIS_UI_LISTEN"}} {
		if c.url == "" || strings.Contains(c.url, c.taken) {
			t.Fatalf("served at %q, want a free port, not %s", c.url, c.taken)
		}
		if res, err := http.Get(c.url); err != nil {
			t.Fatalf("nothing answers at %s: %v", c.url, err)
		} else {
			res.Body.Close()
		}
		if !strings.Contains(logs.String(), c.taken+" is in use") || !strings.Contains(logs.String(), c.env) {
			t.Errorf("the log doesn't say %s was in use and that %s chooses: %s", c.taken, c.env, logs.String())
		}
	}
}

// An address the environment chose is the one wanted: taken, it fails.
func TestServeFailsOnATakenPortTheEnvironmentChose(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	t.Setenv("VIS_LISTEN", taken.Addr().String())
	t.Setenv("VIS_UI_LISTEN", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := vis.NewInstance().Serve(ctx); err == nil || !strings.Contains(err.Error(), "VIS_LISTEN") {
		t.Fatalf("Serve on a taken VIS_LISTEN = %v, want an error naming it", err)
	}
}
