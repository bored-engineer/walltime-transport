package walltime

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPer(t *testing.T) {
	got := Per(90*time.Second, 60*time.Second)
	want := rate.Limit(1.5 * float64(time.Second))
	if got != want {
		t.Fatalf("Per(90s, 60s) = %v, want %v", got, want)
	}
}

// TestTransportThrottles verifies that once the walltime budget is
// exhausted, subsequent requests are delayed rather than allowed through
// immediately.
func TestTransportThrottles(t *testing.T) {
	const requestDuration = 20 * time.Millisecond

	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		time.Sleep(requestDuration)
		return &http.Response{StatusCode: http.StatusOK}, nil
	})

	// Budget: 20ms of walltime per 200ms window, with enough burst to
	// cover a single request's pessimistic estimate.
	tr := New(Per(requestDuration, 200*time.Millisecond), requestDuration, WithTransport(next), WithEstimate(requestDuration))

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := tr.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip #%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// 3 requests costing 20ms each against a budget of 20ms/200ms means
	// the 2nd and 3rd requests must each wait out most of a 200ms window.
	if elapsed < 350*time.Millisecond {
		t.Fatalf("expected throttling to stretch 3 requests past 350ms, took %s", elapsed)
	}
}

// TestTransportRefundsUnusedReservation verifies that when actual request
// walltime is much less than Estimate, the unused portion of the
// reservation is returned to the limiter so it doesn't needlessly throttle
// later requests.
func TestTransportRefundsUnusedReservation(t *testing.T) {
	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	})

	// Estimate pessimistically reserves 100ms per request, but requests
	// actually complete instantly, so the budget should barely be
	// touched after the refund.
	tr := New(Per(100*time.Millisecond, time.Second), 200*time.Millisecond, WithTransport(next), WithEstimate(100*time.Millisecond))

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := tr.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip #%d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Fatalf("expected refunded reservations to avoid throttling, took %s", elapsed)
	}
}

// TestTransportNoEstimate verifies that with no Estimate configured,
// requests reserve nothing upfront (no wait before starting) and are only
// charged after the fact.
func TestTransportNoEstimate(t *testing.T) {
	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	})

	tr := New(Per(time.Millisecond, time.Second), time.Second, WithTransport(next))
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)

	start := time.Now()
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("expected no upfront wait without an estimate, took %s", elapsed)
	}
}

// TestTransportConcurrent verifies the Transport is safe for concurrent
// use, as required of any http.RoundTripper.
func TestTransportConcurrent(t *testing.T) {
	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		time.Sleep(time.Millisecond)
		return &http.Response{StatusCode: http.StatusOK}, nil
	})

	tr := New(Per(50*time.Millisecond, 100*time.Millisecond), 50*time.Millisecond, WithTransport(next), WithEstimate(5*time.Millisecond))
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.RoundTrip(req); err != nil {
				t.Errorf("RoundTrip: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestTransportOffset verifies that Offset is subtracted from a request's
// measured walltime before it's charged against Limiter.
func TestTransportOffset(t *testing.T) {
	const offset = 20 * time.Millisecond
	const sleep = 25 * time.Millisecond

	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		time.Sleep(sleep)
		return &http.Response{StatusCode: http.StatusOK}, nil
	})

	// A generous rate/burst so the request itself is never delayed; only
	// the post-request charge to Limiter's tokens is under test.
	tr := New(rate.Limit(1e6), time.Second, WithTransport(next), WithOffset(offset))

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)

	before := tr.Limiter.Tokens()
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	after := tr.Limiter.Tokens()

	consumed := time.Duration(before - after)
	// Expect ~sleep-offset (5ms) to have been charged, not the full
	// sleep (25ms).
	if consumed < 0 || consumed > 15*time.Millisecond {
		t.Fatalf("expected ~%s charged after offsetting %s of latency out of a %s request, got %s", sleep-offset, offset, sleep, consumed)
	}
}

// TestTransportEstimateExceedsBurst verifies that a request whose Estimate
// asks for a bigger reservation than the limiter's burst can ever grant
// fails immediately with an error.
func TestTransportEstimateExceedsBurst(t *testing.T) {
	next := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK}, nil
	})

	tr := New(Per(time.Second, time.Second), 10*time.Millisecond, WithTransport(next), WithEstimate(time.Second))

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("expected an error when Estimate exceeds the limiter burst")
	}
}
