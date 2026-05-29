// Package pool: circuit breaker for retry storm prevention
package pool

import (
	"sync"
	"time"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState int

const (
	StateClosed CircuitState = iota // Normal operation
	StateOpen                        // Circuit is open, requests fail fast
	StateHalfOpen                    // Testing if service recovered
)

// CircuitBreaker prevents retry storms by failing fast when error rate is high.
type CircuitBreaker struct {
	mu sync.RWMutex

	// Configuration
	maxFailures  int           // Consecutive failures before opening
	openTimeout  time.Duration // How long to stay open before half-open
	halfOpenMax  int           // Max requests allowed in half-open state
	resetTimeout time.Duration // Success window to reset failure count

	// State
	state            CircuitState
	failures         int
	lastFailureTime  time.Time
	lastStateChange  time.Time
	halfOpenAttempts int
	lastSuccessTime  time.Time
}

// NewCircuitBreaker creates a circuit breaker with default settings.
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		maxFailures:  5,
		openTimeout:  30 * time.Second,
		halfOpenMax:  3,
		resetTimeout: 60 * time.Second,
		state:        StateClosed,
	}
}

// Allow checks if a request should be allowed through.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()

	switch cb.state {
	case StateClosed:
		// Reset failure count if we've had recent success
		if !cb.lastSuccessTime.IsZero() && now.Sub(cb.lastSuccessTime) > cb.resetTimeout {
			cb.failures = 0
		}
		return true

	case StateOpen:
		// Check if we should transition to half-open
		if now.Sub(cb.lastStateChange) >= cb.openTimeout {
			cb.state = StateHalfOpen
			cb.halfOpenAttempts = 0
			cb.lastStateChange = now
			return true
		}
		return false

	case StateHalfOpen:
		// Allow limited requests to test recovery
		if cb.halfOpenAttempts < cb.halfOpenMax {
			cb.halfOpenAttempts++
			return true
		}
		return false

	default:
		return false
	}
}

// RecordSuccess records a successful request.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	cb.lastSuccessTime = now

	switch cb.state {
	case StateClosed:
		cb.failures = 0

	case StateHalfOpen:
		// Successful request in half-open state -> close circuit
		cb.state = StateClosed
		cb.failures = 0
		cb.halfOpenAttempts = 0
		cb.lastStateChange = now
	}
}

// RecordFailure records a failed request.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	cb.lastFailureTime = now
	cb.failures++

	switch cb.state {
	case StateClosed:
		if cb.failures >= cb.maxFailures {
			cb.state = StateOpen
			cb.lastStateChange = now
		}

	case StateHalfOpen:
		// Failure in half-open state -> reopen circuit
		cb.state = StateOpen
		cb.halfOpenAttempts = 0
		cb.lastStateChange = now
	}
}

// State returns the current circuit state.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Stats returns circuit breaker statistics.
func (cb *CircuitBreaker) Stats() (state CircuitState, failures int, halfOpenAttempts int) {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state, cb.failures, cb.halfOpenAttempts
}
