package circuitbreaker

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestStateChangeHook_OncePerTransition walks a breaker through a full cycle
// and requires the hook to see each transition exactly once, in order, and
// nothing for calls that change no state.
func TestStateChangeHook_OncePerTransition(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	cb := New(2, 1, 1, 10*time.Second)
	cb.SetNowForTest(clock.Now)
	var seen []string
	cb.SetStateChangeHook(func(from, to State) {
		seen = append(seen, fmt.Sprintf("%s->%s", from, to))
	})

	cb.RecordFailure() // 1 of 2: no transition
	cb.RecordFailure() // closed -> open
	cb.RecordFailure() // already open: nothing
	_ = cb.State()     // timeout not elapsed: nothing
	clock.Advance(11 * time.Second)
	_ = cb.State() // open -> half_open, reported on observation
	_ = cb.State() // already half-open: nothing
	adm, ok := cb.Admit()
	if !ok {
		t.Fatal("half-open breaker refused its probe")
	}
	adm.Failure() // half_open -> open
	clock.Advance(11 * time.Second)
	adm, ok = cb.Admit() // open -> half_open
	if !ok {
		t.Fatal("half-open breaker refused its probe")
	}
	adm.Success()      // half_open -> closed
	cb.RecordSuccess() // closed: nothing

	want := []string{
		"closed->open",
		"open->half_open",
		"half_open->open",
		"open->half_open",
		"half_open->closed",
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("transitions = %v, want %v", seen, want)
	}
}

// TestStateChangeHook_StaleAdmissionReportsNothing: an outcome from a call
// admitted in an earlier generation is ignored, so it must not be reported as
// a transition either.
func TestStateChangeHook_StaleAdmissionReportsNothing(t *testing.T) {
	t.Parallel()

	cb := New(1, 1, 1, time.Minute)
	stale, _ := cb.Admit()
	cb.RecordFailure() // closed -> open
	calls := 0
	cb.SetStateChangeHook(func(State, State) { calls++ })
	stale.Failure()
	stale.Success()
	if calls != 0 {
		t.Fatalf("a stale admission reported %d transitions", calls)
	}
}

func TestStateChangeHook_NilRemoves(t *testing.T) {
	t.Parallel()

	cb := New(1, 1, 1, time.Minute)
	calls := 0
	cb.SetStateChangeHook(func(State, State) { calls++ })
	cb.SetStateChangeHook(nil)
	cb.RecordFailure()
	if calls != 0 {
		t.Fatal("a removed hook was still called")
	}
}
