package otel

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ferro-labs/ai-gateway/observability"
	"go.opentelemetry.io/otel/trace/noop"
)

// deadlineRecordingExporter holds its first Export until gate is closed, and
// records for every later one whether the context it was handed carries a
// deadline.
type deadlineRecordingExporter struct {
	gate    chan struct{}
	entered chan struct{}

	mu           sync.Mutex
	calls        int
	withDeadline int
	noDeadline   int
}

func (e *deadlineRecordingExporter) Name() string                                   { return "deadline-recording" }
func (e *deadlineRecordingExporter) Init(_ context.Context, _ map[string]any) error { return nil }
func (e *deadlineRecordingExporter) Shutdown(_ context.Context) error               { return nil }
func (e *deadlineRecordingExporter) Export(ctx context.Context, _ observability.Event) error {
	e.mu.Lock()
	e.calls++
	first := e.calls == 1
	e.mu.Unlock()
	if first {
		close(e.entered)
		<-e.gate
		return nil
	}
	_, ok := ctx.Deadline()
	e.mu.Lock()
	if ok {
		e.withDeadline++
	} else {
		e.noDeadline++
	}
	e.mu.Unlock()
	return nil
}

// TestShutdown_EveryDrainedEventCarriesTheShutdownDeadline is the regression
// test for drained events escaping the shutdown deadline.
//
// The worker's select picks at random among ready cases, so once Shutdown had
// closed done with events still queued, the steady-state case kept winning
// about half the time and exported those events under a context with no
// deadline — an exporter that honours its context could then hold the drain
// open indefinitely. Every event the worker takes after done is closed now
// carries the shutdown deadline.
//
// Each round holds the worker inside one Export while Shutdown closes done, so
// the queued events behind it are all taken afterwards. Twenty rounds leave the
// old behaviour a one-in-a-million chance of passing.
func TestShutdown_EveryDrainedEventCarriesTheShutdownDeadline(t *testing.T) {
	const rounds, queued = 20, 5
	for round := range rounds {
		ex := &deadlineRecordingExporter{gate: make(chan struct{}), entered: make(chan struct{})}
		p := newProvider(noop.NewTracerProvider(), DefaultConfig())
		p.AttachExporters([]observability.Exporter{ex})

		p.RecordEvent(context.Background(), observability.Event{Subject: "gateway.request.completed"})
		<-ex.entered
		for range queued {
			p.RecordEvent(context.Background(), observability.Event{Subject: "gateway.request.completed"})
		}

		p.mu.RLock()
		done := p.done
		p.mu.RUnlock()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result := make(chan error, 1)
		go func() { result <- p.Shutdown(ctx) }()
		<-done // Shutdown has begun the drain.
		close(ex.gate)
		err := <-result
		cancel()
		if err != nil {
			t.Fatalf("round %d: Shutdown returned %v, want nil", round, err)
		}

		ex.mu.Lock()
		with, without := ex.withDeadline, ex.noDeadline
		ex.mu.Unlock()
		if with+without != queued {
			t.Fatalf("round %d: %d queued events were exported, want %d", round, with+without, queued)
		}
		if without != 0 {
			t.Fatalf("round %d: %d of %d events drained after Shutdown began were exported without its deadline",
				round, without, queued)
		}
	}
}

// contextBoundExporter blocks every Export until its context ends — an exporter
// that honours ctx, posting to a backend that has stopped answering. release
// lets the test unblock it regardless, so nothing outlives the test.
type contextBoundExporter struct {
	entered  chan struct{}
	returned chan error
	release  chan struct{}
}

func (e *contextBoundExporter) Name() string                                   { return "context-bound" }
func (e *contextBoundExporter) Init(_ context.Context, _ map[string]any) error { return nil }
func (e *contextBoundExporter) Shutdown(_ context.Context) error               { return nil }
func (e *contextBoundExporter) Export(ctx context.Context, _ observability.Event) error {
	close(e.entered)
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-e.release:
		err = errors.New("released by the test")
	}
	e.returned <- err
	return err
}

// TestShutdown_CancelsAnExportInFlightAtTheDeadline covers the Export already
// running when shutdown begins. It was taken off the queue before done was
// closed, so it ran under context.Background(): an exporter honouring its
// context still never returned, its call and the worker outlived Shutdown, and
// the exporter's own Shutdown then ran beside an Export that would never end.
// Shutdown now cancels that context once it stops waiting, so a ctx-aware
// exporter returns at the deadline.
func TestShutdown_CancelsAnExportInFlightAtTheDeadline(t *testing.T) {
	ex := &contextBoundExporter{
		entered:  make(chan struct{}),
		returned: make(chan error, 1),
		release:  make(chan struct{}),
	}
	t.Cleanup(func() { close(ex.release) })

	p := newProvider(noop.NewTracerProvider(), DefaultConfig())
	p.AttachExporters([]observability.Exporter{ex})
	p.RecordEvent(context.Background(), observability.Event{Subject: "gateway.request.completed"})
	<-ex.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown returned %v, want the unfinished drain reported as context.DeadlineExceeded", err)
	}

	select {
	case err := <-ex.returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the in-flight Export returned %v, want its context cancelled by Shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an Export in flight when shutdown began was never cancelled: a ctx-aware exporter outlived the shutdown deadline")
	}
}
