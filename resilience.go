package omdb

import (
	"context"
	"sync"
	"time"
)

// This file is a self-contained copy of github.com/icco/gutil/httpx. It is
// duplicated rather than imported on purpose: this package is public, and
// making an outside adopter take a module requirement on a personal utility
// library to get a rate limiter is a poor trade. The two copies serve different
// audiences and have no reason to stay in step.

// rateLimiter is a sliding-window rate limiter. OMDb's free tier is a daily
// quota rather than a burst limit, so this exists to keep a batch from
// hammering the service, not to satisfy a documented per-second cap.
type rateLimiter struct {
	mu       sync.Mutex
	requests []time.Time
	max      int
	window   time.Duration
}

func newRateLimiter(maxRequests int, window time.Duration) *rateLimiter {
	if maxRequests < 1 {
		maxRequests = 1
	}
	return &rateLimiter{max: maxRequests, window: window}
}

// allow reports whether a request fits inside the current window.
func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	// A trailing window covers (now-window, now], so an event exactly window old
	// has aged out and its capacity is free again.
	for len(rl.requests) > 0 && now.Sub(rl.requests[0]) >= rl.window {
		rl.requests = rl.requests[1:]
	}
	if len(rl.requests) < rl.max {
		rl.requests = append(rl.requests, now)
		return true
	}
	return false
}

// wait blocks until a request can be made or ctx is done.
func (rl *rateLimiter) wait(ctx context.Context) error {
	for !rl.allow() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

type circuitState int

const (
	closed circuitState = iota
	open
	halfOpen
)

// circuitBreaker trips after repeated failures and admits a single trial call
// once its timeout expires.
type circuitBreaker struct {
	mu          sync.Mutex
	state       circuitState
	failures    int
	lastFailure time.Time
	maxFailures int
	timeout     time.Duration

	// trialInFlight gates half-open to one probe; without it every concurrent
	// caller is admitted the moment the timeout expires, which is the full
	// pre-breaker load aimed at a service not yet shown healthy. trialStart
	// bounds the gate so a probe that never reports back cannot wedge the
	// breaker shut.
	trialInFlight bool
	trialStart    time.Time
}

func newCircuitBreaker(maxFailures int, timeout time.Duration) *circuitBreaker {
	if maxFailures < 1 {
		maxFailures = 1
	}
	return &circuitBreaker{maxFailures: maxFailures, timeout: timeout}
}

// canExecute reports whether the breaker permits a request.
func (cb *circuitBreaker) canExecute() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	switch cb.state {
	case closed:
		return true
	case open:
		if now.Sub(cb.lastFailure) >= cb.timeout {
			cb.state = halfOpen
			cb.trialInFlight = true
			cb.trialStart = now
			return true
		}
		return false
	case halfOpen:
		if !cb.trialInFlight || now.Sub(cb.trialStart) >= cb.timeout {
			cb.trialInFlight = true
			cb.trialStart = now
			return true
		}
		return false
	default:
		return false
	}
}

func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
	cb.state = closed
	cb.trialInFlight = false
}

func (cb *circuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()
	// A failure in half-open always re-opens: the trial existed to answer
	// exactly this question.
	if cb.state == halfOpen || cb.failures >= cb.maxFailures {
		cb.state = open
	}
	cb.trialInFlight = false
}
