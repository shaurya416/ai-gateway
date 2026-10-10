package mcp

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// allowedToolsWarning is the message initServer logs for allowed_tools
// entries that match nothing.
const allowedToolsWarning = "mcp: allowed_tools entries match no tool the server advertises"

// initWithAllowedTools initializes one HTTP server advertising tools under the
// given allowlist and returns the registry with everything it logged.
func initWithAllowedTools(t *testing.T, tools []Tool, allowed []string) (*Registry, []map[string]any) {
	t.Helper()
	srv := newMockServer(t, tools)
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	reg := NewRegistry(logger.New(logger.Options{Level: "info", Output: &buf}))
	t.Cleanup(func() { _ = reg.Close() })
	reg.RegisterConfig(ServerConfig{Name: "search", URL: srv.URL, AllowedTools: allowed, TimeoutSeconds: 5})
	reg.InitializeAll(t.Context(), func(name string, err error) {
		t.Fatalf("init %s: %v", name, err)
	})

	logged := buf.String()
	records := make([]map[string]any, 0, strings.Count(logged, "\n"))
	for line := range strings.Lines(logged) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		records = append(records, rec)
	}
	return reg, records
}

// warningFor returns the allowed_tools warning logged for server, or nil.
func warningFor(records []map[string]any, server string) map[string]any {
	for _, rec := range records {
		if rec["msg"] == allowedToolsWarning && rec["server"] == server {
			return rec
		}
	}
	return nil
}

// An allowlist entry naming no tool the server advertises — a typo, or a tool
// the server renamed in an upgrade — exposed nothing for that entry and said
// nothing: the server reported ready, and the model was simply never offered
// the tool the operator configured. The entries that match nothing are named.
func TestRegistryWarnsOnAllowedToolsMatchingNothing(t *testing.T) {
	reg, records := initWithAllowedTools(t,
		[]Tool{{Name: "web_search"}, {Name: "fetch"}},
		[]string{"web_search", "websearch", "websearch"},
	)
	if !reg.IsReady("search") {
		t.Fatal("server is not ready; an unmatched allowlist entry must not fail initialization")
	}
	if got := len(reg.AllTools()); got != 1 {
		t.Fatalf("AllTools = %d, want the one matched tool", got)
	}

	rec := warningFor(records, "search")
	if rec == nil {
		t.Fatalf("no warning names the allowlist entry that matched nothing; logged: %v", records)
	}
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	raw, _ := rec["unmatched"].([]any)
	unmatched := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		unmatched = append(unmatched, s)
	}
	if !slices.Equal(unmatched, []string{"websearch"}) {
		t.Errorf("unmatched = %v, want [websearch], once", rec["unmatched"])
	}
}

// An allowlist that matches in full is the ordinary case and stays quiet.
func TestRegistryAllowedToolsAllMatchedLogsNothing(t *testing.T) {
	_, records := initWithAllowedTools(t,
		[]Tool{{Name: "web_search"}, {Name: "fetch"}},
		[]string{"web_search"},
	)
	if rec := warningFor(records, "search"); rec != nil {
		t.Fatalf("warned although every allowlist entry matched: %v", rec)
	}
}
