//go:build unix

package aigateway

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/mcp"
)

// A registry retired by a reload is still held when something is using it — a
// request, or a handshake still in progress — so its Close returns at once and
// the stdio teardown runs later, when the last holder lets go. Gateway.Close
// waits on the reload's tracked goroutine for exactly that teardown, but the
// goroutine finished as soon as Close returned: a reload shortly before
// shutdown let the process exit with the termination ladder still running and
// the subprocess alive, which is how a server that ignores its stdin closing
// is orphaned.
func TestCloseWaitsForRegistryRetiredByReload(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh to run the stdio server: %v", err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	readFile := filepath.Join(dir, "read")

	// Records its pid, reads the initialize request, says so, and then never
	// answers or reads again: the handshake holds the registry until shutdown
	// cancels it, and closing stdin does not end the process — only the ladder's
	// SIGTERM does.
	script := `echo $$ > "$1"; read -r line; echo x > "$2"; exec sleep 30`
	cfg := config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "openai"}},
		MCPServers: []mcp.ServerConfig{{
			Name:    "slow-exit",
			Command: sh,
			Args:    []string{"-c", script, "sh", pidFile, readFile},
		}},
	}
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = gw.Close() })

	waitForStdioServerFile(t, readFile)
	pid := readStdioServerPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	reloaded := cfg
	reloaded.MCPServers = nil
	if err := gw.ReloadConfig(context.Background(), reloaded); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("stdio server %d still exists after Close returned (signal 0: %v); the retired registry's teardown was not waited for", pid, err)
	}
}

func waitForStdioServerFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared; the stdio server did not receive the initialize request", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readStdioServerPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("pid file holds %q", b)
	}
	return pid
}
