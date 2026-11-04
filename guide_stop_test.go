//go:build unix

package vis_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The guide's main is copied as written, by people and by agents. Catching a
// signal turns off Go's own exit on it, so a main that catches Ctrl-C or
// SIGTERM and doesn't shut its server down ignores both: seen in every agent
// adoption of imageproxy on 2026-11-03, whose deploys would hang until killed.
// Each main in doc.go is built as written and must end on either signal.
func TestGuideMainStopsOnSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the guide's main")
	}
	doc, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	mains := guideMains(string(doc))
	if len(mains) == 0 {
		t.Fatal("no main in doc.go starts vis with signal.NotifyContext")
	}
	for i, body := range mains {
		bin := buildGuideMain(t, i, body)
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(fmt.Sprintf("main%d/%v", i+1, sig), func(t *testing.T) {
				if why := runUntilSignal(t, bin, sig); why != "" {
					t.Errorf("the guide's main %d, built as written: %s\n%s", i+1, why, body)
				}
			})
		}
	}
}

// guideMains finds each code block in doc.go that starts vis under
// signal.NotifyContext, as Go source.
func guideMains(doc string) []string {
	var mains []string
	var block []string
	flush := func() {
		if b := strings.Join(block, "\n"); strings.Contains(b, "signal.NotifyContext") && strings.Contains(b, "vis.Start(") {
			mains = append(mains, b)
		}
		block = nil
	}
	for _, line := range strings.Split(doc, "\n") {
		if code, ok := strings.CutPrefix(line, "//\t"); ok {
			block = append(block, code)
		} else if line != "//" || len(block) == 0 {
			flush()
		}
	}
	flush()
	return mains
}

func buildGuideMain(t *testing.T, i int, body string) string {
	t.Helper()
	body = strings.ReplaceAll(body, "{ ... }", "{ log.Fatal(err) }")
	body = regexp.MustCompile(`vis\.Service\("[^"]*"\)`).ReplaceAllString(body, `vis.Service("guide")`)
	src := `package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/tfcace/vis"
)

var (
	_ = context.Background
	_ = os.Interrupt
	_ = syscall.SIGTERM
	_ = log.Fatal
)

func main() {
	addr := flag.String("addr", "", "")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(http.ResponseWriter, *http.Request) {})
	_, _ = addr, mux
` + body + "\n}\n"
	// Inside the module, so it builds against this vis; "_" keeps it out of ./...
	dir, err := os.MkdirTemp(".", "_guidemain")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), fmt.Sprintf("main%d", i+1))
	if out, err := exec.Command("go", "build", "-o", bin, "./"+dir).CombinedOutput(); err != nil {
		t.Fatalf("the guide's main %d doesn't build: %v\n%s\n%s", i+1, err, out, src)
	}
	return bin
}

// runUntilSignal starts bin serving on a free port, sends it sig, and says
// what went wrong, or "" when it ended within 5 s.
func runUntilSignal(t *testing.T, bin string, sig syscall.Signal) string {
	t.Helper()
	addrs := freeAddrs(t, 3)
	addr := addrs[0]
	cmd := exec.Command(bin, "-addr", addr)
	cmd.Env = append(os.Environ(), "VIS_LISTEN="+addrs[1], "VIS_UI_LISTEN="+addrs[2])
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	defer func() { cmd.Process.Kill(); <-done }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		select {
		case <-done:
			return "it ended before serving: " + out.String()
		default:
		}
		if time.Now().After(deadline) {
			return "it never served on -addr: " + out.String()
		}
	}
	cmd.Process.Signal(sig)
	select {
	case <-done:
		return ""
	case <-time.After(5 * time.Second):
		return fmt.Sprintf("still running 5 s after %v", sig)
	}
}

// freeAddrs is n distinct free addresses: each listener stays open until
// all are chosen. Closed one by one, Linux handed the same port out twice, and
// the guide's main found its own -addr taken by vis's UI.
func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		out = append(out, ln.Addr().String())
	}
	return out
}
