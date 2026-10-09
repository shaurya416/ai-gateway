// Package circuitbreaker implements the circuit-breaker pattern for provider
// calls. Each provider should have its own CircuitBreaker instance.
//
// State transitions:
//
//	Closed → Open        when consecutive failures ≥ FailureThreshold
//	Open   → HalfOpen   after Timeout elapses
//	HalfOpen → Closed   when consecutive successes ≥ SuccessThreshold
//	HalfOpen → Open     on any failure
//
// Every transition starts a new generation. A call admitted through Admit
// carries the generation it was admitted in, and its outcome is applied only
// while that generation is current: a call admitted while Closed that finishes
// after the circuit opened and aged into HalfOpen is not a probe, and must
// neither close the circuit nor free a probe slot it never took.
package circuitbreaker

import (
	"errors"
	"sync"
	"time"
)

// State represents the circuit breaker's current state.
type State int

const (
	// StateClosed — normal operation; requests pass through.
	StateClosed State = iota
	// StateOpen — provider is considered failing; requests are rejected immediately.
	StateOpen
	// StateHalfOpen — circuit is testing recovery with a limited number of requests.
	StateHalfOpen
)

// String implements fmt.Stringer.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// ErrCircuitOpen is returned when a call is rejected because the circuit is open.
var ErrCircuitOpen = errors.New("circuit breaker open")

// CircuitBreaker guards a single downstream provider.
type CircuitBreaker struct {
	mu               sync.Mutex
	state            State
	failureCount     int
	successCount     int
	failureThreshold int
	successThreshold int
	maxHalfThreshold int    // cap on concurrent in-flight probes while half-open
	halfOpenProbes   int    // current number of in-flight probes
	generation       uint64 // advanced on every state transition
	timeout          time.Duration
	openUntil        time.Time
	now              func() time.Time // clock seam; defaults to time.Now, overridable in tests
}

// New creates a CircuitBreaker with the given thresholds and open timeout.
// Defaults are applied for zero/negative values: failureThreshold=5,
// successThreshold=1, timeout=30s.
func New(failureThreshold, successThreshold int, maxHalfThreshold int, timeout time.Duration) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if successThreshold <= 0 {
		successThreshold = 1
	}
	if maxHalfThreshold <= 0 {
		maxHalfThreshold = 1
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		maxHalfThreshold: maxHalfThreshold,
		timeout:          timeout,
		now:              time.Now,
	}
}

// SetNowForTest overrides the internal clock used for Open→HalfOpen timeout
// transitions so tests (including those in other packages) can advance virtual
// time deterministically instead of sleeping. Passing nil restores time.Now.
func (cb *CircuitBreaker) SetNowForTest(fn func() time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if fn == nil {
		fn = time.Now
	}
	cb.now = fn
}

// State returns the current state, transitioning Open→HalfOpen if the timeout
// has elapsed.
func (cb *CircuitBreaker) State() State {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.resolveState()
}

// resolveState must be called with cb.mu held.
func (cb *CircuitBreaker) resolveState() State {
	if cb.state == StateOpen && cb.now().After(cb.openUntil) {
		cb.setState(StateHalfOpen)
		cb.successCount = 0
		cb.halfOpenProbes = 0
	}
	return cb.state
}

// setState moves to s and starts a new generation. Must be called with cb.mu
// held.
func (cb *CircuitBreaker) setState(s State) {
	cb.state = s
	cb.generation++
}

// Admission is the receipt for one call Admit let through. It records the
// generation the call was admitted in and whether it took a half-open probe
// slot, so its outcome can be applied to that generation and no other.
//
// Resolve every admission exactly once — Success, Failure or Release. The
// zero Admission belongs to no breaker, and resolving it does nothing.
type Admission struct {
	cb         *CircuitBreaker
	generation uint64
	probe      bool
}

// Admit is Allow with a receipt: ok is false when the call must be rejected,
// and otherwise the returned Admission resolves the call's outcome.
func (cb *CircuitBreaker) Admit() (Admission, bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if !cb.allowLocked() {
		return Admission{}, false
	}
	return Admission{cb: cb, generation: cb.generation, probe: cb.state == StateHalfOpen}, true
}

// Breaker returns the breaker that issued the admission, or nil for the zero
// Admission.
func (a Admission) Breaker() *CircuitBreaker { return a.cb }

// Success records the admitted call as a success. It is ignored once the
// generation that admitted the call has ended.
func (a Admission) Success() {
	if a.cb == nil {
		return
	}
	a.cb.mu.Lock()
	defer a.cb.mu.Unlock()
	if a.generation == a.cb.generation {
		a.cb.recordSuccessLocked()
	}
}

// Failure records the admitted call as a failure. It is ignored once the
// generation that admitted the call has ended: a call admitted before the
// circuit opened says nothing about a half-open circuit's probes.
func (a Admission) Failure() {
	if a.cb == nil {
		return
	}
	a.cb.mu.Lock()
	defer a.cb.mu.Unlock()
	if a.generation == a.cb.generation {
		a.cb.recordFailureLocked()
	}
}

// Release resolves the admitted call without recording success or failure,
// returning its half-open probe slot if it took one. A call that took no slot,
// or whose generation has ended, releases nothing.
func (a Admission) Release() {
	if a.cb == nil || !a.probe {
		return
	}
	a.cb.mu.Lock()
	defer a.cb.mu.Unlock()
	if a.generation == a.cb.generation {
		a.cb.releaseProbeLocked()
	}
}

// Allow returns true if the request should proceed (circuit is Closed or
// HalfOpen), false if it should be rejected (circuit is Open).
//
// The outcome of a call admitted here is reported through RecordSuccess,
// RecordFailure or ReleaseProbe, which apply to whatever generation is current
// when they run. A caller whose calls can outlive a state transition should use
// Admit instead.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.allowLocked()
}

// allowLocked must be called with cb.mu held.
func (cb *CircuitBreaker) allowLocked() bool {
	if cb.resolveState() == StateOpen {
		return false
	}
	if cb.state == StateHalfOpen {
		if cb.halfOpenProbes >= cb.maxHalfThreshold {
			return false
		}
		cb.halfOpenProbes++
	}
	return true
}

// ReleaseProbe releases an admitted half-open probe without recording success
// or failure. It is used when the gateway intentionally ignores an outcome,
// such as rate limits or caller-side cancellation.
func (cb *CircuitBreaker) ReleaseProbe() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.releaseProbeLocked()
}

// releaseProbeLocked must be called with cb.mu held.
func (cb *CircuitBreaker) releaseProbeLocked() {
	if cb.state == StateHalfOpen && cb.halfOpenProbes > 0 {
		cb.halfOpenProbes--
	}
}

// RecordSuccess notifies the breaker that a call succeeded.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.recordSuccessLocked()
}

// recordSuccessLocked must be called with cb.mu held.
func (cb *CircuitBreaker) recordSuccessLocked() {
	switch cb.state {
	case StateHalfOpen:
		if cb.halfOpenProbes > 0 {
			cb.halfOpenProbes--
		}
		cb.successCount++
		if cb.successCount >= cb.successThreshold {
			cb.setState(StateClosed)
			cb.failureCount = 0
			cb.successCount = 0
			cb.halfOpenProbes = 0
		}
	case StateClosed:
		cb.failureCount = 0
	}
}

// RecordFailure notifies the breaker that a call failed.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.recordFailureLocked()
}

// recordFailureLocked must be called with cb.mu held.
func (cb *CircuitBreaker) recordFailureLocked() {
	switch cb.state {
	case StateClosed:
		cb.failureCount++
		if cb.failureCount >= cb.failureThreshold {
			cb.setState(StateOpen)
			cb.openUntil = cb.now().Add(cb.timeout)
		}
	case StateHalfOpen:
		cb.setState(StateOpen)
		cb.openUntil = cb.now().Add(cb.timeout)
		cb.successCount = 0
		cb.halfOpenProbes = 0
	}
}
