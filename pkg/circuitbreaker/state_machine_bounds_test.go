package circuitbreaker

import (
	"testing"
	"time"
)

// New's documented defaults apply to a zero value: five consecutive failures
// open the circuit, and it stays open for thirty seconds.
func TestNew_ZeroValuesTakeTheDocumentedDefaults(t *testing.T) {
	t.Parallel()

	cb := New(0, 0, 0, 0)
	clk := newFakeClock()
	cb.SetNowForTest(clk.Now)

	for i := 1; i < 5; i++ {
		cb.RecordFailure()
		if cb.State() != StateClosed {
			t.Fatalf("opened after %d failures; the default threshold is 5", i)
		}
	}
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open after the fifth failure, got %s", cb.State())
	}

	clk.Advance(29 * time.Second)
	if cb.State() != StateOpen {
		t.Fatalf("expected open 29s in; the default timeout is 30s, got %s", cb.State())
	}
	clk.Advance(2 * time.Second)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half_open 31s in, got %s", cb.State())
	}
}

// A circuit that closes from half-open starts counting consecutive failures
// from zero: one failure after recovery is not FailureThreshold failures.
func TestCircuitBreaker_RecoveredCircuitNeedsAFullRunOfFailuresToReopen(t *testing.T) {
	t.Parallel()

	cb := New(3, 1, 1, time.Second)
	clk := newFakeClock()
	cb.SetNowForTest(clk.Now)

	for range 3 {
		cb.RecordFailure()
	}
	if cb.State() != StateOpen {
		t.Fatalf("expected open, got %s", cb.State())
	}
	clk.Advance(time.Minute)
	probe, ok := cb.Admit()
	if !ok {
		t.Fatal("half-open circuit rejected its probe")
	}
	probe.Success()
	if cb.State() != StateClosed {
		t.Fatalf("expected closed after the probe succeeded, got %s", cb.State())
	}

	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != StateClosed {
		t.Fatalf("two failures after recovery reopened a circuit whose threshold is 3, got %s", cb.State())
	}
	cb.RecordFailure()
	if cb.State() != StateOpen {
		t.Fatalf("expected open after three consecutive failures, got %s", cb.State())
	}
}

// Releasing a probe admitted in an earlier half-open period frees nothing:
// the slot it took went with that period, and freeing one now would let the
// current period run more probes than maxHalfThreshold allows.
func TestAdmission_StaleProbeReleaseFreesNoSlot(t *testing.T) {
	t.Parallel()

	cb := New(1, 1, 2, time.Second)
	clk := newFakeClock()
	cb.SetNowForTest(clk.Now)
	openThenHalfOpen(t, cb, clk)

	stale, _ := cb.Admit()
	failing, _ := cb.Admit()
	failing.Failure()
	clk.Advance(time.Minute)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half_open, got %s", cb.State())
	}

	for i := range 2 {
		if _, ok := cb.Admit(); !ok {
			t.Fatalf("half-open circuit rejected probe %d of 2", i+1)
		}
	}
	stale.Release()
	if _, ok := cb.Admit(); ok {
		t.Fatal("a third concurrent probe was admitted after a probe from an earlier period was released")
	}
}

// ReleaseProbe with no probe in flight frees nothing, so it cannot raise the
// half-open cap.
func TestCircuitBreaker_ReleaseProbeWithoutAProbeKeepsTheCap(t *testing.T) {
	t.Parallel()

	cb := New(1, 1, 1, time.Second)
	clk := newFakeClock()
	cb.SetNowForTest(clk.Now)
	cb.RecordFailure()
	clk.Advance(time.Minute)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half_open, got %s", cb.State())
	}

	cb.ReleaseProbe()
	if !cb.Allow() {
		t.Fatal("half-open circuit rejected its probe")
	}
	if cb.Allow() {
		t.Fatal("a second concurrent probe was admitted under a cap of 1")
	}
}

// SetNowForTest(nil) restores the wall clock.
func TestSetNowForTest_NilRestoresTheWallClock(t *testing.T) {
	t.Parallel()

	cb := New(1, 1, 1, time.Millisecond)
	cb.SetNowForTest(func() time.Time { return time.Unix(0, 0) })
	cb.SetNowForTest(nil)
	cb.RecordFailure()
	time.Sleep(5 * time.Millisecond)
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half_open once the wall clock passed the timeout, got %s", cb.State())
	}
}
