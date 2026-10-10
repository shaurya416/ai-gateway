package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pagedToolsServer answers tools/list one page at a time. pages maps the cursor
// a request carries ("" for the first request) to the tools on that page and
// the nextCursor it hands back.
func pagedToolsServer(t *testing.T, pages map[string]struct {
	tools []string
	next  string
}) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu      sync.Mutex
		cursors []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case mcpMethodInitialize:
			_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: initializeResult("paged", "1")})
		case mcpMethodToolsList:
			mu.Lock()
			cursors = append(cursors, req.Params.Cursor)
			mu.Unlock()
			page, ok := pages[req.Params.Cursor]
			if !ok {
				_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID,
					Error: &JSONRPCError{Code: -32602, Message: "invalid cursor"}})
				return
			}
			tools := make([]Tool, len(page.tools))
			for i, n := range page.tools {
				tools[i] = Tool{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)}
			}
			result := map[string]any{"tools": tools}
			if page.next != "" {
				result["nextCursor"] = page.next
			}
			_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: mustMarshal(result)})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), cursors...)
	}
}

// tools/list is paginated. The HTTP client read the first page and stopped, so
// every tool past it was silently missing: the server reported ready, and an
// allowed_tools entry naming a later tool matched nothing — while the stdio
// transport, whose library follows nextCursor, saw the same server's tools in
// full.
func TestClientListToolsFollowsNextCursor(t *testing.T) {
	srv, cursors := pagedToolsServer(t, map[string]struct {
		tools []string
		next  string
	}{
		"":       {tools: []string{"alpha", "beta"}, next: "page-2"},
		"page-2": {tools: []string{"gamma"}, next: "page-3"},
		"page-3": {tools: []string{"delta"}},
	})

	c := NewClient(srv.URL, nil, 5*time.Second)
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	if got, want := strings.Join(names, ","), "alpha,beta,gamma,delta"; got != want {
		t.Errorf("tools = %s, want %s — every page of tools/list, in order", got, want)
	}
	if got, want := strings.Join(cursors(), ","), ",page-2,page-3"; got != want {
		t.Errorf("cursors sent = %q, want %q — each request must carry the previous page's nextCursor", got, want)
	}
}

// The same through the registry: a tool on a later page is advertised and
// resolvable, and an allowed_tools entry can name it.
func TestRegistryDiscoversToolsOnLaterPages(t *testing.T) {
	srv, _ := pagedToolsServer(t, map[string]struct {
		tools []string
		next  string
	}{
		"":       {tools: []string{"alpha"}, next: "page-2"},
		"page-2": {tools: []string{"search"}},
	})

	reg := NewRegistry(nil)
	reg.RegisterConfig(ServerConfig{Name: "paged", URL: srv.URL, TimeoutSeconds: 5, AllowedTools: []string{"search"}})
	reg.InitializeAll(context.Background(), func(name string, err error) {
		t.Errorf("init %s: %v", name, err)
	})
	t.Cleanup(func() { _ = reg.Close() })

	all := reg.AllTools()
	if len(all) != 1 || all[0].Name != "search" {
		t.Fatalf("AllTools = %+v, want the allow-listed tool from the second page", all)
	}
	if !reg.Owns("search") {
		t.Error("the second page's tool does not resolve")
	}
}

// Page size is the server's choice. A server paging two hundred tools five at a
// time is a legitimate configuration, and the bound on runaway listings must
// not refuse it: refusing fails the server's initialization, which loses every
// tool, where reading only the first page had at least kept five.
func TestClientListToolsFollowsAListingOfManySmallPages(t *testing.T) {
	const pageCount, perPage = 40, 5
	pages := map[string]struct {
		tools []string
		next  string
	}{}
	for p := range pageCount {
		cursor := ""
		if p > 0 {
			cursor = "page-" + strconv.Itoa(p)
		}
		var page struct {
			tools []string
			next  string
		}
		for i := range perPage {
			page.tools = append(page.tools, "tool-"+strconv.Itoa(p*perPage+i))
		}
		if p+1 < pageCount {
			page.next = "page-" + strconv.Itoa(p+1)
		}
		pages[cursor] = page
	}
	srv, _ := pagedToolsServer(t, pages)

	tools, err := NewClient(srv.URL, nil, 5*time.Second).ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools over %d pages: %v", pageCount, err)
	}
	if got, want := len(tools), pageCount*perPage; got != want {
		t.Errorf("tools = %d, want %d", got, want)
	}
}

// A server that hands out cursors forever fails discovery rather than holding
// the handshake open and growing the tool list until it times out.
func TestClientListToolsBoundsPageCount(t *testing.T) {
	var (
		mu    sync.Mutex
		pages int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		pages++
		n := pages
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: mustMarshal(map[string]any{
			"tools":      []Tool{{Name: "t", InputSchema: json.RawMessage(`{}`)}},
			"nextCursor": "again-" + strings.Repeat("x", n%3),
		})})
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, nil, 5*time.Second).ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools succeeded against a server whose pages never end")
	}
	mu.Lock()
	got := pages
	mu.Unlock()
	if got != maxToolListPages {
		t.Errorf("pages requested = %d, want %d", got, maxToolListPages)
	}
}

// The page bound is set high so a legitimate listing of many small pages is
// never refused, which leaves memory to a bound of its own: the listing as a
// whole is held to the byte limit a single response has. A server handing out
// large pages without end fails once their total passes it, long before the
// page bound is reached.
func TestClientListToolsBoundsListingSize(t *testing.T) {
	const perPage = maxResponseBodyBytes / 8
	var (
		mu    sync.Mutex
		pages int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		pages++
		n := pages
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: mustMarshal(map[string]any{
			"tools":      []Tool{{Name: "t" + strconv.Itoa(n), Description: strings.Repeat("x", perPage), InputSchema: json.RawMessage(`{}`)}},
			"nextCursor": "page-" + strconv.Itoa(n+1),
		})})
	}))
	t.Cleanup(srv.Close)

	_, err := NewClient(srv.URL, nil, 5*time.Second).ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools succeeded against a server whose listing never ends")
	}
	mu.Lock()
	got := pages
	mu.Unlock()
	// Seven pages hold seven eighths of the limit; the eighth passes it.
	if got != 8 {
		t.Errorf("pages requested = %d, want 8 — the listing must stop once its total size passes %d bytes", got, maxResponseBodyBytes)
	}
}
