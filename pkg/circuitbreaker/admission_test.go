package circuitbreaker

import (
	"testing"
	"time"
)

// openThenHalfOpen trips cb with a call admitted for that purpose and ages it
// into half-open.
func openThenHalfOpen(t *testing.T, cb *CircuitBreaker, clk *fakeClock) {
	t.Helper()
	trip, ok := cb.Admit()
	if !ok {
		t.Fatal("closed circuit rejected a call")
	}
	trip.Failure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open, got %s", cb.State())
	}
	clk.Advance(time.Minute)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half_open, got %s", cb.State())
	}
}

// A call admitted while Closed that finishes after the circuit aged into
// HalfOpen is not a probe: its outcome belongs to a generation that has ended.
func TestAdmission_PreOutageOutcomeDoesNotResolveHalfOpen(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		resolve func(Admission)
	}{
		{"success", Admission.Success},
		{"failure", Admission.Failure},
		{"release", Admission.Release},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cb := New(1, 1, 1, time.Second)
			clk := newFakeClock()
			cb.SetNowForTest(clk.Now)

			preOutage, ok := cb.Admit()
			if !ok {
				t.Fatal("closed circuit rejected a call")
			}
			openThenHalfOpen(t, cb, clk)
			probe, ok := cb.Admit()
			if !ok {
				t.Fatal("half-open circuit rejected its first probe")
			}

			tc.resolve(preOutage)

			if cb.State() != StateHalfOpen {
				t.Fatalf("expected half_open after a pre-outage %s, got %s", tc.name, cb.State())
			}
			if _, ok := cb.Admit(); ok {
				t.Fatalf("a pre-outage %s freed the probe slot the in-flight probe holds", tc.name)
			}

			probe.Success()
			if cb.State() != StateClosed {
				t.Fatalf("expected closed after the probe succeeded, got %s", cb.State())
			}
		})
	}
}

// A probe from an earlier half-open period, still in flight when a sibling
// probe reopened the circuit, must not decide the next half-open period.
func TestAdmission_ProbeFromEarlierHalfOpenIsIgnored(t *testing.T) {
	t.Parallel()

	cb := New(1, 1, 2, time.Second)
	clk := newFakeClock()
	cb.SetNowForTest(clk.Now)
	openThenHalfOpen(t, cb, clk)

	slow, _ := cb.Admit()
	failing, _ := cb.Admit()
	failing.Failure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open after a probe failed, got %s", cb.State())
	}
	clk.Advance(time.Minute)
	current, ok := cb.Admit()
	if !ok {
		t.Fatal("half-open circuit rejected its first probe")
	}

	slow.Success()
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half_open: a probe from the previous period closed the circuit, got %s", cb.State())
	}

	current.Success()
	if cb.State() != StateClosed {
		t.Fatalf("expected closed after the current probe succeeded, got %s", cb.State())
	}
}

// Within one generation an admission resolves exactly as the untracked
// methods do.
func TestAdmission_CurrentGenerationResolvesNormally(t *testing.T) {
	t.Parallel()

	cb := New(2, 1, 1, time.Second)
	clk := newFakeClock()
	cb.SetNowForTest(clk.Now)

	a, _ := cb.Admit()
	b, _ := cb.Admit()
	a.Failure()
	if cb.State() != StateClosed {
		t.Fatalf("expected closed below the threshold, got %s", cb.State())
	}
	b.Failure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open at the threshold, got %s", cb.State())
	}

	clk.Advance(time.Minute)
	probe, ok := cb.Admit()
	if !ok {
		t.Fatal("half-open circuit rejected its first probe")
	}
	probe.Release()
	if _, ok := cb.Admit(); !ok {
		t.Fatal("a released probe did not return its slot")
	}
}

// The zero Admission belongs to no breaker; resolving it is a no-op.
func TestAdmission_ZeroValueIsInert(t *testing.T) {
	t.Parallel()

	var a Admission
	if a.Breaker() != nil {
		t.Fatal("zero Admission names a breaker")
	}
	a.Success()
	a.Failure()
	a.Release()
}
