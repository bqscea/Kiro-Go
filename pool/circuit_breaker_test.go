// Package pool: circuit breaker tests
package pool

import (
	"testing"
	"time"
)

func TestCircuitBreakerClosed(t *testing.T) {
	cb := NewCircuitBreaker()

	// Initially closed, should allow requests
	if !cb.Allow() {
		t.Fatal("expected circuit to allow request when closed")
	}

	// Record success, should stay closed
	cb.RecordSuccess()
	if state := cb.State(); state != StateClosed {
		t.Fatalf("expected state=Closed, got %v", state)
	}
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.maxFailures = 3

	// Record failures
	for i := 0; i < 3; i++ {
		cb.RecordFailure()
	}

	// Should be open now
	if state := cb.State(); state != StateOpen {
		t.Fatalf("expected state=Open after %d failures, got %v", cb.maxFailures, state)
	}

	// Should reject requests
	if cb.Allow() {
		t.Fatal("expected circuit to reject request when open")
	}
}

func TestCircuitBreakerHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.maxFailures = 2
	cb.openTimeout = 100 * time.Millisecond
	cb.halfOpenMax = 2

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	if state := cb.State(); state != StateOpen {
		t.Fatalf("expected state=Open, got %v", state)
	}

	// Wait for half-open transition
	time.Sleep(150 * time.Millisecond)

	// First request should transition to half-open
	if !cb.Allow() {
		t.Fatal("expected circuit to allow first request in half-open")
	}

	if state := cb.State(); state != StateHalfOpen {
		t.Fatalf("expected state=HalfOpen, got %v", state)
	}

	// Should allow second request (halfOpenMax=2)
	if !cb.Allow() {
		t.Fatal("expected circuit to allow second request in half-open")
	}

	// Third request should be rejected (exceeded halfOpenMax)
	if cb.Allow() {
		t.Fatal("expected circuit to reject third request after half-open limit")
	}
}

func TestCircuitBreakerRecovery(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.maxFailures = 2
	cb.openTimeout = 50 * time.Millisecond

	// Open the circuit
	cb.RecordFailure()
	cb.RecordFailure()

	// Wait for half-open
	time.Sleep(100 * time.Millisecond)

	// Allow request
	if !cb.Allow() {
		t.Fatal("expected circuit to allow request in half-open")
	}

	// Record success -> should close
	cb.RecordSuccess()

	if state := cb.State(); state != StateClosed {
		t.Fatalf("expected state=Closed after success in half-open, got %v", state)
	}

	// Should allow requests again
	if !cb.Allow() {
		t.Fatal("expected circuit to allow request after recovery")
	}
}

func TestCircuitBreakerStats(t *testing.T) {
	cb := NewCircuitBreaker()

	state, failures, halfOpen := cb.Stats()
	if state != StateClosed || failures != 0 || halfOpen != 0 {
		t.Fatalf("expected initial stats (Closed, 0, 0), got (%v, %d, %d)", state, failures, halfOpen)
	}

	cb.RecordFailure()
	cb.RecordFailure()

	state, failures, halfOpen = cb.Stats()
	if failures != 2 {
		t.Fatalf("expected 2 failures, got %d", failures)
	}
}
