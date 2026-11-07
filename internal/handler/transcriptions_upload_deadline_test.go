package handler

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/middleware"
	"github.com/ferro-labs/ai-gateway/providers"
)

// uploadSizeProvider records how many audio bytes reached it.
type uploadSizeProvider struct{ received atomic.Int64 }

func (*uploadSizeProvider) Name() string                { return "stt" }
func (*uploadSizeProvider) SupportsModel(m string) bool { return m == "whisper-1" }
func (*uploadSizeProvider) Complete(context.Context, providers.Request) (*providers.Response, error) {
	return &providers.Response{}, nil
}
func (p *uploadSizeProvider) Transcribe(_ context.Context, req providers.TranscriptionRequest) (*providers.TranscriptionResponse, error) {
	p.received.Store(int64(len(req.File)))
	return &providers.TranscriptionResponse{Text: "ok"}, nil
}

// The audio routes accept an upload up to 25 MiB, but the server's ReadTimeout
// bounds the whole request, body included, so a file still arriving when it
// elapsed was refused 400 "i/o timeout" however far under the cap it was: at
// thirty seconds, a 25 MiB upload needs about 7 Mbit/s just to be read. The
// upload is bounded by idleness instead — a gap longer than ReadTimeout still
// ends it, a long steady one does not.
func TestTranscriptions_UploadIsBoundedByIdlenessNotByDuration(t *testing.T) {
	const (
		readTimeout = 300 * time.Millisecond
		chunkSize   = 4 << 10
	)

	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stt", Models: []string{"whisper-1"}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stub := &uploadSizeProvider{}
	gw.RegisterProvider(stub)

	srv := httptest.NewUnstartedServer(Transcriptions(gw, false))
	srv.Config.ReadTimeout = readTimeout
	srv.Config.ReadHeaderTimeout = readTimeout
	srv.Start()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	defer client.Transport.(*http.Transport).CloseIdleConnections()

	// upload streams the audio file one chunk per gap. A refused upload may reach
	// the client as the gateway's error status or, when the gateway has already
	// closed the connection, as a failed write; status is 0 for the latter.
	upload := func(t *testing.T, gaps []time.Duration) (status int, sent int64) {
		t.Helper()
		stub.received.Store(-1)
		pr, pw := io.Pipe()
		form := multipart.NewWriter(pw)
		done := make(chan struct{})
		go func() {
			defer close(done)
			part, err := form.CreateFormFile("file", "speech.wav")
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			chunk := bytes.Repeat([]byte{0x55}, chunkSize)
			for _, gap := range gaps {
				if _, err := part.Write(chunk); err != nil {
					return
				}
				time.Sleep(gap)
			}
			if err := form.WriteField("model", "whisper-1"); err != nil {
				return
			}
			_ = pw.CloseWithError(form.Close())
		}()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/audio/transcriptions", pr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", form.FormDataContentType())
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
		if got := stub.received.Load(); got != sent {
			t.Fatalf("provider received %d audio bytes, want all %d", got, sent)
		}
	})

	t.Run("an upload that stalls past ReadTimeout is still cut off", func(t *testing.T) {
		status, _ := upload(t, []time.Duration{3 * readTimeout, 0})
		if status == http.StatusOK {
			t.Fatalf("status = 200, want the stalled upload refused")
		}
		if got := stub.received.Load(); got != -1 {
			t.Fatalf("a stalled upload reached the provider (%d bytes)", got)
		}
	})
}

// slowTranscriber takes delay to answer, as a real transcription of a long file
// does.
type slowTranscriber struct {
	uploadSizeProvider
	delay time.Duration
}

func (p *slowTranscriber) Transcribe(ctx context.Context, req providers.TranscriptionRequest) (*providers.TranscriptionResponse, error) {
	time.Sleep(p.delay)
	return p.uploadSizeProvider.Transcribe(ctx, req)
}

// The server's WriteTimeout is armed when the request's headers are read, so
// the time an upload spends arriving comes out of the answer's budget. Once a
// slow upload is read to the end rather than cut off, a transcription that took
// longer than what was left would be produced, billed and then lost on the
// wire. The answer gets the whole WriteTimeout from its first byte. Re-arming
// it is the router's job, not the handler's, so the handler is served behind
// the middleware the router installs.
func TestTranscriptions_AnswerGetsTheWholeWriteTimeoutAfterASlowUpload(t *testing.T) {
	const (
		readTimeout  = 300 * time.Millisecond
		writeTimeout = 1100 * time.Millisecond
		chunkSize    = 4 << 10
		chunks       = 10 // ~1s to arrive
	)

	gw, err := newTestGateway(t, config.Config{
		Strategy: config.StrategyConfig{Mode: config.ModeSingle},
		Targets:  []config.Target{{VirtualKey: "stt", Models: []string{"whisper-1"}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stub := &slowTranscriber{delay: 400 * time.Millisecond}
	gw.RegisterProvider(stub)

	srv := httptest.NewUnstartedServer(middleware.WriteTimeoutFromAnswer(Transcriptions(gw, false)))
	srv.Config.ReadTimeout = readTimeout
	srv.Config.ReadHeaderTimeout = readTimeout
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	defer client.Transport.(*http.Transport).CloseIdleConnections()

	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)
	done := make(chan struct{})
	go func() {
		defer close(done)
		part, err := form.CreateFormFile("file", "speech.wav")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		chunk := bytes.Repeat([]byte{0x55}, chunkSize)
		for range chunks {
			if _, err := part.Write(chunk); err != nil {
				return
			}
			time.Sleep(readTimeout / 3)
		}
		if err := form.WriteField("model", "whisper-1"); err != nil {
			return
		}
		_ = pw.CloseWithError(form.Close())
	}()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/audio/transcriptions", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := client.Do(req)
	_ = pr.CloseWithError(io.ErrClosedPipe)
	<-done
	if err != nil {
		t.Fatalf("upload: %v: the transcription was produced and its answer lost", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if got, want := stub.received.Load(), int64(chunks*chunkSize); got != want {
		t.Fatalf("provider received %d audio bytes, want all %d", got, want)
	}
}
