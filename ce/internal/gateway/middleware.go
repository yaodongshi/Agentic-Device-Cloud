package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"adc.dev/ce/internal/httpx"
)

// RateLimiter is the minimal rate-limit contract the gateway depends on.
// It is defined locally so this package never imports the limiter
// implementation (no dependency cycle; the real limiter plugs in at
// assembly time, LLD 3.5.2). A nil RateLimiter disables gateway-level
// limiting.
type RateLimiter interface {
	// Allow reports whether one request in scope (e.g. "global" or a route
	// prefix) identified by key (e.g. the client IP) may proceed.
	Allow(ctx context.Context, scope, key string) (bool, error)
}

// RateLimitInfo reports window details for the X-RateLimit-* response
// headers (design/33 1.9). Limiters implementing only the minimal
// RateLimiter contract degrade to Remaining=0 plus Retry-After on denial.
type RateLimitInfo struct {
	Limit     int64
	Remaining int64
	Reset     time.Time // window reset time; zero means unknown
}

// present reports whether the info carries header-worthy values.
func (i RateLimitInfo) present() bool {
	return i.Limit > 0 || i.Remaining > 0 || !i.Reset.IsZero()
}

// rateLimiterInfo is an optional extension of RateLimiter that also
// reports window details, enabling accurate X-RateLimit-* headers on both
// allowed and denied requests.
type rateLimiterInfo interface {
	AllowInfo(ctx context.Context, scope, key string) (allowed bool, info RateLimitInfo, err error)
}

// ctxTraceIDKey carries the trace id in the request context (LLD 3.5.1).
type ctxTraceIDKey struct{}

// TraceID ensures X-Trace-ID and X-ADC-Request-ID exist on every inbound
// request, generates them when the client omitted them, and echoes both
// plus X-ADC-Version on every response (design/33 1.9; the trace id must
// propagate across the whole chain, LLD 3.5.1).
func TraceID(version string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			traceID := r.Header.Get("X-Trace-ID")
			if traceID == "" {
				traceID = newID()
				r.Header.Set("X-Trace-ID", traceID)
			}
			// X-ADC-Request-ID passes through unchanged when present
			// (design/33 1.9); it is generated only when the client
			// omitted it.
			requestID := r.Header.Get("X-ADC-Request-ID")
			if requestID == "" {
				requestID = newID()
				r.Header.Set("X-ADC-Request-ID", requestID)
			}
			ctx := context.WithValue(r.Context(), ctxTraceIDKey{}, traceID)
			w = &traceWriter{ResponseWriter: w, traceID: traceID, requestID: requestID, version: version}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// newID returns a 32-hex-char random identifier (UUID format without
// dashes, matching the trace_id examples in design/33).
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// traceWriter stamps trace identifiers onto the first response header
// write; values already set downstream (e.g. echoed by a backend) win.
// Hijack and Flush pass through so WebSocket upgrades and streaming keep
// working behind the wrapper (ReverseProxy requires http.Hijacker for 101).
type traceWriter struct {
	http.ResponseWriter
	traceID   string
	requestID string
	version   string
	wrote     bool
}

func (w *traceWriter) WriteHeader(status int) {
	if !w.wrote {
		h := w.Header()
		if h.Get("X-Trace-ID") == "" {
			h.Set("X-Trace-ID", w.traceID)
		}
		if h.Get("X-ADC-Request-ID") == "" {
			h.Set("X-ADC-Request-ID", w.requestID)
		}
		if w.version != "" && h.Get("X-ADC-Version") == "" {
			h.Set("X-ADC-Version", w.version)
		}
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *traceWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Hijack passes connection hijacking through to the wrapped writer so
// WebSocket upgrades (/v1/devices/tunnel) survive the trace middleware.
func (w *traceWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("gateway: %T does not support hijacking", w.ResponseWriter)
	}
	return hj.Hijack()
}

// Flush passes flushing through so streaming responses are forwarded
// immediately (FlushInterval=-1 on the proxy).
func (w *traceWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// requestID returns the X-ADC-Request-ID of the request; the TraceID
// middleware guarantees it is set before the inner handlers run.
func requestID(r *http.Request) string {
	return r.Header.Get("X-ADC-Request-ID")
}

// authHeaderNames lists the credential-bearing headers the gateway
// forwards verbatim (design/33 1.2 auth matrix). The gateway never parses
// business token semantics — tenant and role decisions stay in the backend
// services (GAP-13 same-source principle, LLD 3.5.1).
var authHeaderNames = []string{
	"Authorization",
	"Cookie",
	"X-ADC-Key",
	"X-Device-ID",
	"X-ADC-Timestamp",
	"X-ADC-Nonce",
	"X-ADC-Signature",
}

type ctxAuthHeadersKey struct{}

// PassAuth snapshots the credential headers of the inbound request into
// the context so the proxy can re-apply them verbatim downstream. It
// deliberately does not parse, validate or store credentials — the gateway
// must not become a second tenant authority (LLD 3.5.1, GAP-13).
func PassAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := make(http.Header)
		for _, name := range authHeaderNames {
			if vs := r.Header.Values(name); len(vs) > 0 {
				h[http.CanonicalHeaderKey(name)] = cloneHeader(vs)
			}
		}
		// Any X-Device-* header is device-credential material and must be
		// forwarded untouched (design/33 1.3 tunnel HMAC).
		for name, vs := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Device-") {
				h[name] = cloneHeader(vs)
			}
		}
		ctx := context.WithValue(r.Context(), ctxAuthHeadersKey{}, h)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func cloneHeader(vs []string) []string {
	out := make([]string, len(vs))
	copy(out, vs)
	return out
}

// applyAuthHeaders re-applies the credential headers captured by PassAuth
// onto an outbound proxy request (idempotent; called from Rewrite).
func applyAuthHeaders(ctx context.Context, out http.Header) {
	h, _ := ctx.Value(ctxAuthHeadersKey{}).(http.Header)
	for name, vs := range h {
		out.Del(name)
		for _, v := range vs {
			out.Add(name, v)
		}
	}
}

// RateLimit applies gateway-level limiting: a global entry bucket
// (anti-DDoS) plus a per-route-prefix bucket resolved by scopeFor
// (LLD 3.5.2). Business-dimension limiting (tenant/agent/device) stays in
// the backend services (SEC-12). A nil limiter disables the middleware.
func RateLimit(lim RateLimiter, scopeFor func(path string) string, errorLog *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if lim == nil {
				next.ServeHTTP(w, r)
				return
			}
			key := clientIP(r)
			scopes := []string{"global"}
			if scope := scopeFor(r.URL.Path); scope != "" {
				scopes = append(scopes, scope)
			}
			for _, scope := range scopes {
				allowed, info, err := allowWithInfo(lim, r.Context(), scope, key)
				if err != nil {
					// Limiter infrastructure failure: fail open — the
					// gateway bucket is anti-DDoS only, backends keep
					// their business-dimension limits (SEC-12).
					if errorLog != nil {
						errorLog.Printf("gateway: rate limiter error on scope %s: %v", scope, err)
					}
					continue
				}
				if info.present() {
					setRateLimitHeaders(w.Header(), info)
				}
				if !allowed {
					w.Header().Set("X-RateLimit-Remaining", "0")
					writeRateLimited(w, r, info)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// allowWithInfo prefers the extended contract when the limiter implements
// it, falling back to the minimal RateLimiter.
func allowWithInfo(lim RateLimiter, ctx context.Context, scope, key string) (bool, RateLimitInfo, error) {
	if li, ok := lim.(rateLimiterInfo); ok {
		return li.AllowInfo(ctx, scope, key)
	}
	allowed, err := lim.Allow(ctx, scope, key)
	return allowed, RateLimitInfo{}, err
}

// setRateLimitHeaders writes X-RateLimit-Limit/Remaining/Reset per
// design/33 1.9.
func setRateLimitHeaders(h http.Header, info RateLimitInfo) {
	if info.Limit > 0 {
		h.Set("X-RateLimit-Limit", strconv.FormatInt(info.Limit, 10))
	}
	if info.Remaining > 0 {
		h.Set("X-RateLimit-Remaining", strconv.FormatInt(info.Remaining, 10))
	}
	if !info.Reset.IsZero() {
		h.Set("X-RateLimit-Reset", strconv.FormatInt(info.Reset.Unix(), 10))
	}
}

// writeRateLimited emits 429 + code 10006 + Retry-After (design/33 1.9).
func writeRateLimited(w http.ResponseWriter, r *http.Request, info RateLimitInfo) {
	if !info.Reset.IsZero() && info.Reset.After(time.Now()) {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(time.Until(info.Reset).Seconds())+1, 10))
	} else {
		w.Header().Set("Retry-After", "1")
	}
	httpx.WriteError(w, http.StatusTooManyRequests, "10006", "rate limit exceeded", requestID(r))
}

// clientIP extracts the client address for rate-limit keys. Behind the
// edge nginx (design/60) the real client IP is the first X-Forwarded-For
// value; without a trusted proxy it falls back to the TCP peer address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
