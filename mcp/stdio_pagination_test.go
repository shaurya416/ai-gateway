package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// startHelperClient launches the helper in mode and completes the handshake,
// leaving tools/list to the test.
func startHelperClient(t *testing.T, name, mode string) mcpClient {
	t.Helper()
	cfg := helperServerConfig(t, name, mode)
	client := newStdioClient(cfg.Name, cfg.Command, cfg.Args, cfg.Env)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return client
}

// A stdio server that hands out a cursor on every page must fail its discovery
// at the page bound. Following cursors without one held the handshake open —
// and appended every page to memory — until the initialization deadline, the
// unbounded walk the HTTP transport already refuses.
func TestStdioListToolsBoundsPageCount(t *testing.T) {
	client := startHelperClient(t, "endless", helperModeEndlessCursor)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err == nil {
		t.Fatalf("ListTools succeeded with %d tools from a listing that never ends", len(tools))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ListTools ran until the deadline instead of stopping at the page bound: %v", err)
	}
	if !strings.Contains(err.Error(), "pages") {
		t.Fatalf("error does not name the page bound: %v", err)
	}
}

// The listing as a whole is held to the same byte bound as the HTTP transport,
// so pages that are each well under the per-message cap cannot accumulate past
// it.
func TestStdioListToolsBoundsListingSize(t *testing.T) {
	client := startHelperClient(t, "large-pages", helperModeEndlessLargePages)

	// Reaching the bound moves about 10 MiB through the subprocess and decodes
	// it twice, which takes several seconds under the race detector. The
	// deadline only has to outlast that: a listing that is not bounded runs
	// into it whatever its length, so a generous one separates the two
	// outcomes just as well and does not fail a loaded CI runner.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err == nil {
		t.Fatalf("ListTools succeeded with %d tools past the listing size bound", len(tools))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ListTools ran until the deadline instead of stopping at the size bound: %v", err)
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("error does not name the size bound: %v", err)
	}
}

// Following the cursor must still return a listing that ends.
func TestStdioListToolsReturnsTerminatingListing(t *testing.T) {
	client := startHelperClient(t, "single-page", helperModeServe)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want the helper's 3", len(tools))
	}
	if tools[0].Name != helperToolEcho || len(tools[0].InputSchema) == 0 {
		t.Fatalf("first tool = %+v, want %s with its schema", tools[0], helperToolEcho)
	}
}
