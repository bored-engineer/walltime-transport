// Package walltime provides an http.RoundTripper that throttles outgoing
// requests using golang.org/x/time/rate, based on the cumulative walltime
// spent in flight rather than the number of requests.
package walltime

import (
	"fmt"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// Per returns the rate.Limit that permits d of request walltime to elapse
// for every period of real time. For example:
//
//	walltime.Per(90*time.Second, 60*time.Second)
//
// permits at most 90 seconds of cumulative request walltime for every 60
// seconds that pass.
func Per(d, period time.Duration) rate.Limit {
	return rate.Limit(float64(d) / period.Seconds())
}

// Transport is an http.RoundTripper that delays requests, using Limiter, so
// that the cumulative walltime spent inside RoundTrip (from when a request
// starts to when it finishes, successfully or not) does not exceed
// Limiter's configured rate.
//
// Each token in Limiter represents one nanosecond of request walltime.
type Transport struct {
	// Limiter is consulted before every request and charged for the
	// actual walltime each request takes.
	Limiter *rate.Limiter
	// Transport is used to perform requests. http.DefaultTransport is
	// used if nil.
	Transport http.RoundTripper
	// Offset is subtracted from each request's measured walltime before
	// it's charged against Limiter, to exclude a fixed amount of network
	// latency (e.g. typical RTT) that isn't real work being throttled.
	// The charged cost is never reduced below zero.
	Offset time.Duration
	// Estimate is a pessimistic upper bound on how long a request will
	// take, reserved from Limiter before the request starts. Once the
	// request finishes, the reservation is trued up to its actual cost.
	Estimate time.Duration
}

// Option configures a Transport constructed by New.
type Option func(*Transport)

// WithTransport sets the http.RoundTripper used to perform requests.
// http.DefaultTransport is used if this option isn't given.
func WithTransport(next http.RoundTripper) Option {
	return func(t *Transport) {
		t.Transport = next
	}
}

// WithOffset sets Offset, the fixed amount subtracted from each request's
// measured walltime before it's charged against Limiter.
func WithOffset(offset time.Duration) Option {
	return func(t *Transport) {
		t.Offset = offset
	}
}

// WithEstimate sets Estimate, the pessimistic upper bound on request
// duration reserved from Limiter before each request starts.
func WithEstimate(estimate time.Duration) Option {
	return func(t *Transport) {
		t.Estimate = estimate
	}
}

// New returns a Transport using a rate.Limiter constructed from r and b
// exactly as rate.NewLimiter would.
func New(r rate.Limit, b time.Duration, opts ...Option) *Transport {
	t := &Transport{
		Limiter: rate.NewLimiter(r, int(b)),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Transport
	if next == nil {
		next = http.DefaultTransport
	}

	now := time.Now()
	pre := t.Limiter.ReserveN(now, int(t.Estimate))
	if !pre.OK() {
		return nil, fmt.Errorf("walltime: estimate of %s exceeds limiter burst of %d", t.Estimate, t.Limiter.Burst())
	}
	if delay := pre.DelayFrom(now); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-req.Context().Done():
			pre.CancelAt(time.Now())
			return nil, req.Context().Err()
		}
	}

	start := time.Now()
	resp, err := next.RoundTrip(req)
	elapsed := time.Since(start) - t.Offset
	if elapsed < 0 {
		elapsed = 0
	}

	// True up the pessimistic reservation to the actual walltime spent:
	// diff is negative (returning tokens) if the request finished faster
	// than Estimate, or positive (consuming more tokens) if it ran over.
	// Reservation.CancelAt can't be used here since it only refunds a
	// reservation that hasn't come due yet, which is never true once a
	// request has already completed.
	if diff := elapsed - t.Estimate; diff != 0 {
		t.Limiter.ReserveN(time.Now(), int(diff))
	}

	return resp, err
}
