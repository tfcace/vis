package vis

import (
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServer is the instance's MCP server without its HTTP transport, for
// tests that connect in memory (inside a synctest bubble, say). Read tools only.
func MCPServer(in *Instance) *mcp.Server {
	s, _ := in.mcpServer(mcpOptions{})
	return s
}

// SetDefaultAddrs replaces the addresses Serve uses when nothing chooses
// them, for tests that can't rely on 7070 and 7071 being free or taken.
func SetDefaultAddrs(mcp, ui string) (restore func()) {
	m, u := defaultMCPAddr, defaultUIAddr
	defaultMCPAddr, defaultUIAddr = mcp, ui
	return func() { defaultMCPAddr, defaultUIAddr = m, u }
}

// StallPeers makes in sit on every peer request for d, as a paused process would.
func StallPeers(in *Instance, d time.Duration) { in.stallPeers.Store(int64(d)) }

// StallCalls makes in sit on forwarded tool calls, handshakes included, for
// d, while its gossip goes on: a member starved of CPU, slow but alive.
func StallCalls(in *Instance, d time.Duration) { in.stallCalls.Store(int64(d)) }

// StallSummaries makes in answer the fleet's summary calls after d, as an
// instance too starved for CPU to answer in time would.
func StallSummaries(in *Instance, d time.Duration) { in.stallSummary.Store(int64(d)) }

// SkipLeave stops in from telling its peers it is leaving when it stops, as a
// crash, a kill or a hang would.
func SkipLeave(in *Instance) { in.skipLeave.Store(true) }

// StageOrder is the order a rollout of knob changes members in.
func StageOrder(names []string, knob string) []string { return stageOrder(names, knob) }

// Settled ages in past its first minute, for tests of measurement that
// don't mean to test the warm-up.
func Settled(in *Instance) *Instance { in.started = in.started.Add(-time.Hour); return in }

// FakeCgroup points the cgroup reader at a fixture for the test: a cgroup v2
// at /test whose files are those given (name to contents).
func FakeCgroup(t interface {
	TempDir() string
	Cleanup(func())
}, files map[string]string) {
	root, proc := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "test")
	os.MkdirAll(dir, 0o755)
	for name, body := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	os.WriteFile(filepath.Join(proc, "cgroup"), []byte("0::/test\n"), 0o644)
	oldRoot, oldSelf := cgroupPaths()
	setCgroupPaths(root, filepath.Join(proc, "cgroup"))
	t.Cleanup(func() { setCgroupPaths(oldRoot, oldSelf) })
}
