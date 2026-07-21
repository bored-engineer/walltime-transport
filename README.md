# walltime-transport [![Go Reference](https://pkg.go.dev/badge/github.com/bored-engineer/walltime-transport.svg)](https://pkg.go.dev/github.com/bored-engineer/walltime-transport)
A Golang [http.RoundTripper](https://pkg.go.dev/net/http#RoundTripper) that delays/rate-limits HTTP requests based on the wall-clock time the server spends processing the request and (starting) to return a response.

Unlike a typical rate-limiter that counts *requests*, this counts the cumulative *wall-clock time* spent inside `RoundTrip` (from when a request starts to when it returns a status/headers) and throttles once that exceeds a configured [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) budget.

## Usage
For example, GitHub's REST API enforces a [secondary rate limit](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits) of no more than 90 seconds of CPU/wall time per 60 second window which can be easily represented using `walltime.Transport`:

```go
package main

import (
	"net/http"
	"time"

	walltime "github.com/bored-engineer/walltime-transport"
)

func main() {
	client := &http.Client{
		Transport: walltime.New(
			// Allow 90s of cumulative request walltime per 60s window.
			walltime.Per(90*time.Second, 60*time.Second),
			// Burst size: let a client use its entire 90s budget at once.
			90*time.Second,
			// Pessimistically reserve 10s per request before it starts,
			// the maximum time before GitHub itself times out a REST API
			// request; the reservation is trued up to the actual
			// duration once the request completes.
			walltime.WithEstimate(10*time.Second),
			// Subtract ~30ms of estimated network latency from each
			// request's measured walltime before charging it, since
			// that wasn't CPU/wall time spent processing the request.
			walltime.WithOffset(30*time.Millisecond),
		),
	}

	resp, err := client.Get("https://api.github.com/octocat")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
}
```

### Options

- `WithTransport(http.RoundTripper)` — the underlying `http.RoundTripper` used to perform requests. Defaults to `http.DefaultTransport`.
- `WithEstimate(time.Duration)` — a pessimistic upper bound on request duration, reserved before each request starts and trued up afterward. Defaults to `0`, meaning nothing is reserved upfront and requests are only charged after they complete.
- `WithOffset(time.Duration)` — a fixed amount subtracted from each request's measured walltime before it's charged, to exclude latency (e.g. typical RTT) that isn't real work being throttled.
