package httpapi

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Health checks are unauthenticated so load balancers and orchestrators can
// call them; they reveal nothing beyond up or down.

// healthz answers whether the process is alive. It never checks the
// database: if Postgres is down, restarting the service wouldn't help.
func (h *handler) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz answers whether the service can serve requests: the database is
// reachable, the schema is up to date, and the server isn't shutting down.
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	fail := func(reason string) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": reason})
	}
	if h.cfg.Ready != nil && !h.cfg.Ready() {
		fail("shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.svc.DB().PingContext(ctx); err != nil {
		h.cfg.Logger.WarnContext(ctx, "readiness check failed", "err", err)
		fail("database unreachable")
		return
	}
	if err := h.svc.CheckSchema(ctx); err != nil {
		h.cfg.Logger.WarnContext(ctx, "readiness check failed", "err", err)
		fail("database schema is not up to date")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// metrics collects request counts and latencies per route, exposed in the
// Prometheus text format on GET /metrics.
type metrics struct {
	mu       sync.Mutex
	requests map[[2]string]uint64 // (route, status code) → count
	latency  map[string]*histogram
	inFlight int64
}

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type histogram struct {
	counts []uint64 // one per bucket, cumulative when written out
	sum    float64
	count  uint64
}

func newMetrics() *metrics {
	return &metrics{requests: map[[2]string]uint64{}, latency: map[string]*histogram{}}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// instrument records each request to next under its route pattern.
func (m *metrics) instrument(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		m.mu.Lock()
		m.inFlight++
		m.mu.Unlock()
		defer func() {
			d := time.Since(start).Seconds()
			m.mu.Lock()
			defer m.mu.Unlock()
			m.inFlight--
			m.requests[[2]string{route, fmt.Sprint(rec.status)}]++
			hg := m.latency[route]
			if hg == nil {
				hg = &histogram{counts: make([]uint64, len(latencyBuckets))}
				m.latency[route] = hg
			}
			for i, b := range latencyBuckets {
				if d <= b {
					hg.counts[i]++
					break
				}
			}
			hg.sum += d
			hg.count++
		}()
		next.ServeHTTP(rec, r)
	})
}

func (h *handler) metricsHandler(w http.ResponseWriter, r *http.Request) {
	m := h.metrics
	var b strings.Builder
	m.mu.Lock()
	b.WriteString("# HELP ledger_http_requests_total HTTP requests by route and status code.\n# TYPE ledger_http_requests_total counter\n")
	reqKeys := make([][2]string, 0, len(m.requests))
	for k := range m.requests {
		reqKeys = append(reqKeys, k)
	}
	slices.SortFunc(reqKeys, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })
	for _, k := range reqKeys {
		fmt.Fprintf(&b, "ledger_http_requests_total{route=%q,code=%q} %d\n", k[0], k[1], m.requests[k])
	}
	b.WriteString("# HELP ledger_http_request_duration_seconds HTTP request latency by route.\n# TYPE ledger_http_request_duration_seconds histogram\n")
	routes := make([]string, 0, len(m.latency))
	for r := range m.latency {
		routes = append(routes, r)
	}
	slices.Sort(routes)
	for _, route := range routes {
		hg := m.latency[route]
		var cum uint64
		for i, le := range latencyBuckets {
			cum += hg.counts[i]
			fmt.Fprintf(&b, "ledger_http_request_duration_seconds_bucket{route=%q,le=%q} %d\n", route, fmt.Sprint(le), cum)
		}
		fmt.Fprintf(&b, "ledger_http_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", route, hg.count)
		fmt.Fprintf(&b, "ledger_http_request_duration_seconds_sum{route=%q} %g\n", route, hg.sum)
		fmt.Fprintf(&b, "ledger_http_request_duration_seconds_count{route=%q} %d\n", route, hg.count)
	}
	fmt.Fprintf(&b, "# HELP ledger_http_requests_in_flight Requests being served.\n# TYPE ledger_http_requests_in_flight gauge\nledger_http_requests_in_flight %d\n", m.inFlight)
	m.mu.Unlock()

	st := h.svc.DB().Stats()
	fmt.Fprintf(&b, "# HELP ledger_db_connections Database connections by state.\n# TYPE ledger_db_connections gauge\n")
	fmt.Fprintf(&b, "ledger_db_connections{state=\"in_use\"} %d\nledger_db_connections{state=\"idle\"} %d\n", st.InUse, st.Idle)
	fmt.Fprintf(&b, "# HELP ledger_db_wait_total Times a request waited for a free database connection.\n# TYPE ledger_db_wait_total counter\nledger_db_wait_total %d\n", st.WaitCount)
	fmt.Fprintf(&b, "# HELP ledger_db_wait_seconds_total Time spent waiting for free database connections.\n# TYPE ledger_db_wait_seconds_total counter\nledger_db_wait_seconds_total %g\n", st.WaitDuration.Seconds())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// limiter is a token bucket per client IP: each IP may make rate requests per
// second on average, in bursts of up to burst.
type limiter struct {
	rate  float64
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
	calls   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	if rate <= 0 {
		return nil
	}
	if burst < 1 {
		burst = int(math.Ceil(rate))
	}
	return &limiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow reports whether ip may make a request now, and if not, how long until
// it may.
func (l *limiter) allow(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls++; l.calls%1024 == 0 {
		// Forget idle clients so the map can't grow without bound.
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
