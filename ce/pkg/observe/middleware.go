// HTTP metrics middleware (design/80 B-09): request counting and duration
// histogram with hand-rolled buckets. Recording must never break the main
// request path (PRD FR-016), so nil vecs disable metrics silently and a
// status-recording error cannot occur.
package observe

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Middleware records adc_http_requests_total{route,code} and
// adc_http_request_duration_seconds{route,code} around next.
//
// The route label comes from routeFrom; with routeFrom nil the ServeMux
// pattern (http.Request.Pattern, Go 1.22+) is used, falling back to the raw
// path. The status code is captured via a ResponseWriter wrapper (default
// 200). WebSocket upgrades pass through unchanged: the wrapper implements
// Hijacker/Flusher, so the device tunnel keeps working behind the metrics.
// On a hijacked connection the duration covers the whole connection
// lifetime (the tunnel handler blocks until the session ends).
//
// Nil vecs skip recording entirely (nil metrics seam).
func Middleware(requests *CounterVec, duration *HistogramVec, routeFrom func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)

			route := ""
			if routeFrom != nil {
				route = routeFrom(r)
			}
			if route == "" {
				route = defaultRoute(r)
			}
			code := sw.code
			if code == 0 {
				code = http.StatusOK
			}
			elapsed := time.Since(start).Seconds()
			requests.With(route, strconv.Itoa(code)).Inc()
			duration.With(route, strconv.Itoa(code)).Observe(elapsed)
		})
	}
}

// defaultRoute returns the ServeMux pattern when the request was routed by
// one, otherwise the raw path.
func defaultRoute(r *http.Request) string {
	if p := r.Pattern; p != "" {
		return p
	}
	return r.URL.Path
}

// statusWriter captures the first status code written (0 = nothing written
// yet, exported as 200) and passes hijacking/flushing through so WebSocket
// upgrades survive the middleware.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Hijack passes the upgrade through to the wrapped writer (WSS tunnel).
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("observe: %T does not support hijacking", w.ResponseWriter)
	}
	return hj.Hijack()
}

// Flush forwards flushing so streaming responses are not buffered.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
