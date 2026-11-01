package httpserver_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/providers"
)

// filesBackend serves the batch surface from a fixed upstream.
type filesBackend struct{ baseURL string }

func (filesBackend) Name() string              { return "files" }
func (filesBackend) SupportsModel(string) bool { return false }
func (filesBackend) Complete(context.Context, providers.Request) (*providers.Response, error) {
	return &providers.Response{}, nil
}
func (b filesBackend) BatchBaseURL() string { return b.baseURL }
func (filesBackend) BatchAuthHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer upstream-secret"}
}

// An upload outlives the server's ReadTimeout when it arrives steadily, through
// the real router: the read deadline is moved through every response writer the
// middleware stack wraps around the handler's, so one that stopped unwrapping
// would quietly put the whole-request bound back.
func TestRouter_FilesUploadOutlivesReadTimeoutWhileArriving(t *testing.T) {
	const (
		readTimeout = 300 * time.Millisecond
		chunkSize   = 4 << 10
		chunks      = 10
	)

	var received atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received.Store(n)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file-abc","object":"file"}`))
	}))
	t.Cleanup(upstream.Close)

	gw, err := newTestGateway(t, config.Config{
		Strategy:    config.StrategyConfig{Mode: config.ModeSingle},
		Targets:     []config.Target{{VirtualKey: "files"}},
		BatchTarget: "files",
	})
	if err != nil {
		t.Fatalf("New gateway: %v", err)
	}
	gw.RegisterProvider(filesBackend{baseURL: upstream.URL})

	srv := httptest.NewUnstartedServer(buildTestRouter(t, gw))
	srv.Config.ReadTimeout = readTimeout
	srv.Config.ReadHeaderTimeout = readTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		chunk := bytes.Repeat([]byte{'x'}, chunkSize)
		for range chunks {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
			time.Sleep(readTimeout / 3)
		}
		_ = pw.Close()
	}()

	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	t.Cleanup(client.Transport.(*http.Transport).CloseIdleConnections)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/files", pr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	_ = pr.CloseWithError(io.ErrClosedPipe)
	<-done
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if got, want := received.Load(), int64(chunks*chunkSize); got != want {
		t.Fatalf("upstream received %d bytes, want all %d", got, want)
	}
}
