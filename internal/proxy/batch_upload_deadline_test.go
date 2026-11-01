package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// An upload to /v1/files streams straight through to the batch target, outside
// the shared body limit, because a batch input file is far larger than any chat
// body. The server's ReadTimeout bounds the whole request, body included, so it
// capped every upload at whatever the caller could send inside it: a file still
// arriving when it elapsed was cut off mid-body and answered 400 "i/o timeout",
// however steadily it was arriving. An upload is bounded by idleness instead —
// a gap longer than ReadTimeout still ends it, a long steady one does not.
func TestBatchHandler_UploadIsBoundedByIdlenessNotByDuration(t *testing.T) {
	const (
		readTimeout = 300 * time.Millisecond
		chunkSize   = 4 << 10
	)

	var received atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received.Store(n)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file-abc","object":"file"}`))
	}))
	defer upstream.Close()

	src := &batchSourceStub{target: "openai", provider: &batchStub{name: "openai", baseURL: upstream.URL}}
	gateway := httptest.NewUnstartedServer(BatchHandler(src))
	gateway.Config.ReadTimeout = readTimeout
	gateway.Config.ReadHeaderTimeout = readTimeout
	gateway.Start()
	defer gateway.Close()

	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	defer client.Transport.(*http.Transport).CloseIdleConnections()

	// upload sends one chunk per gap, so the body takes len(gaps) gaps to arrive.
	// A refused upload may reach the client as the gateway's error status or,
	// when the gateway has already closed the connection, as a failed write;
	// status is 0 for the latter.
	upload := func(t *testing.T, gaps []time.Duration) (status int, sent int64) {
		t.Helper()
		received.Store(-1)
		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			chunk := bytes.Repeat([]byte{'x'}, chunkSize)
			for _, gap := range gaps {
				if _, err := pw.Write(chunk); err != nil {
					return
				}
				time.Sleep(gap)
			}
			_ = pw.Close()
		}()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gateway.URL+"/v1/files", pr)
		if err != nil {
			t.Fatal(err)
		}
		sent = int64(len(gaps)) * chunkSize
		resp, err := client.Do(req)
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-done
		if err != nil {
			t.Logf("upload: %v", err)
			return 0, sent
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, sent
	}

	t.Run("a steady upload longer than ReadTimeout is served", func(t *testing.T) {
		gaps := make([]time.Duration, 10) // ~1s in all, over three ReadTimeouts
		for i := range gaps {
			gaps[i] = readTimeout / 3
		}
		status, sent := upload(t, gaps)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200: an upload still arriving must not be cut off", status)
		}
		if got := received.Load(); got != sent {
			t.Fatalf("upstream received %d bytes, want all %d", got, sent)
		}
	})

	t.Run("an upload that stalls past ReadTimeout is still cut off", func(t *testing.T) {
		status, sent := upload(t, []time.Duration{3 * readTimeout, 0})
		if status == http.StatusOK {
			t.Fatalf("status = 200, want the stalled upload refused")
		}
		if got := received.Load(); got == sent {
			t.Fatalf("upstream received the whole %d-byte body from a stalled upload", got)
		}
	})
}
