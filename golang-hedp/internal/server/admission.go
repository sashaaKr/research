package server

import (
	"net/http"
	"strconv"
	"sync/atomic"
)

// limitInFlight caps how many render requests a pod accepts at once, and sheds
// the rest with 503 + Retry-After.
//
// This exists because of a measured property of the workload: one pod is
// already saturated by a single client streaming chunks back to back.
// Measured on a 4-core pod, 1000 configs in chunks of 100:
//
//	clients   throughput      per-config p50
//	1         37.8 configs/s  97 ms
//	2         39.1 configs/s  196 ms
//	4         39.0 configs/s  364 ms
//	8         38.9 configs/s  747 ms
//
// Throughput is flat and latency is linear in concurrency - the definition of
// a saturated server. Accepting the 8th concurrent batch does not render one
// extra config per second, it just makes all eight callers wait 8x longer, and
// it multiplies the pod's peak memory by eight.
//
// Shedding instead is strictly better behind a load balancer: a 503 lets the
// caller retry against a replica that is actually idle, which is the only
// thing that adds throughput. Queueing inside the pod hides the need for
// another replica; refusing makes it visible, to the client and to the HPA.
func limitInFlight(limit int, retryAfterSeconds int, next http.Handler) http.Handler {
	if limit <= 0 {
		return next
	}
	var inFlight atomic.Int64
	retryAfter := strconv.Itoa(retryAfterSeconds)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the render paths are expensive. Probes and version management
		// must stay answerable while the pod is at capacity, or a busy pod
		// fails its readiness check and gets pulled out of rotation exactly
		// when it is doing useful work.
		if !isRenderPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		if n := inFlight.Add(1); n > int64(limit) {
			inFlight.Add(-1)
			w.Header().Set("Retry-After", retryAfter)
			encode(w, http.StatusServiceUnavailable, errorResponse{
				Error: "at capacity: " + strconv.Itoa(limit) + " renders already in flight; retry against another replica",
			})
			return
		}
		defer inFlight.Add(-1)

		next.ServeHTTP(w, r)
	})
}

func isRenderPath(path string) bool {
	switch path {
	case "/v1/render", "/v1/render/bulk":
		return true
	}
	return false
}
