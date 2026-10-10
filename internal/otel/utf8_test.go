package otel

import (
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ferro-labs/ai-gateway/observability"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestNonUTF8RequestValueDoesNotDropTheSpanBatch is the regression test for one
// request discarding every span exported alongside it.
//
// OTLP carries attribute values, status descriptions and event attributes as
// protobuf strings, which must be valid UTF-8. The SDK does not check, so a
// value carrying raw bytes — a multipart `model` field, or an error message
// quoting it — reached the exporter as-is, failed to marshal, and the export
// call for the WHOLE batch returned an error: the healthy spans batched with it
// were never sent either.
func TestNonUTF8RequestValueDoesNotDropTheSpanBatch(t *testing.T) {
	c := newCollectorStub(t)
	exporter, err := newSpanExporter(context.Background(), Config{Endpoint: c.URL, Protocol: "http/protobuf"})
	if err != nil {
		t.Fatalf("newSpanExporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	prov := newProvider(tp, DefaultConfig())

	_, healthy := prov.StartRequestSpan(context.Background(), observability.RequestAttrs{RequestModel: "gpt-4o"})
	healthy.End()

	_, poisoned := prov.StartRequestSpan(context.Background(), observability.RequestAttrs{RequestModel: "whisper-\xff1"})
	poisoned.SetError(errors.New(`model "whisper-\xff1" is not served by any target`))
	poisoned.End()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatalf("export failed: %v — one non-UTF-8 value dropped every span in the batch", err)
	}
	select {
	case <-c.paths:
	default:
		t.Fatal("no OTLP export reached the collector")
	}
}

// TestSpanStringsAreValidUTF8 covers each place the provider turns a caller's
// string into span data, so none of them can reintroduce the batch loss above.
func TestSpanStringsAreValidUTF8(t *testing.T) {
	for _, privacy := range []string{PrivacyLevelMetadata, PrivacyLevelFull} {
		t.Run(privacy, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.PrivacyLevel = privacy
			prov, exp := newTestProviderWithConfig(t, cfg)

			ctx, span := prov.StartRequestSpan(context.Background(), observability.RequestAttrs{
				System:        "openai",
				RequestModel:  "req-\xff",
				ResponseModel: "resp-\xfe",
				TargetKey:     "target-\xc3",
				User:          "user-\xff",
				SessionID:     "session-\xff",
				Metadata:      map[string]string{"team-\xff": "value-\xff"},
			})
			span.SetAttribute("ferro.test.string", "set-\xff")
			span.SetAttribute("ferro.test.slice", []string{"ok", "slice-\xff"})
			span.SetAttribute("ferro.test.other", []byte("sprint-\xff"))
			span.SetError(errors.New("upstream said \xff"))
			_, attempt := prov.StartAttemptSpan(ctx, "attempt-\xff", 1)
			attempt.End()
			span.End()

			spans := exp.GetSpans()
			if len(spans) != 2 {
				t.Fatalf("got %d spans, want 2", len(spans))
			}
			for _, s := range spans {
				assertValidUTF8Attrs(t, s.Name, s.Attributes)
				if !utf8.ValidString(s.Status.Description) {
					t.Errorf("%s: status description %q is not valid UTF-8", s.Name, s.Status.Description)
				}
				for _, ev := range s.Events {
					assertValidUTF8Attrs(t, s.Name+"/"+ev.Name, ev.Attributes)
				}
			}
		})
	}
}

func assertValidUTF8Attrs(t *testing.T, where string, attrs []attribute.KeyValue) {
	t.Helper()
	for _, kv := range attrs {
		if !utf8.ValidString(string(kv.Key)) {
			t.Errorf("%s: attribute key %q is not valid UTF-8", where, kv.Key)
		}
		switch kv.Value.Type() {
		case attribute.STRING:
			if !utf8.ValidString(kv.Value.AsString()) {
				t.Errorf("%s: attribute %s = %q is not valid UTF-8", where, kv.Key, kv.Value.AsString())
			}
		case attribute.STRINGSLICE:
			for _, v := range kv.Value.AsStringSlice() {
				if !utf8.ValidString(v) {
					t.Errorf("%s: attribute %s element %q is not valid UTF-8", where, kv.Key, v)
				}
			}
		}
	}
}
