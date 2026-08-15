// Middleware assembly for the shared HTTP transport layer (SEC-19):
// per-request timeout, request body cap and trace id propagation.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HeaderTraceID is the header carrying the request-chain trace id (LLD 1.3.5).
// The TraceID middleware guarantees its presence on every inbound request.
const HeaderTraceID = "X-Trace-ID"

// Unified error codes aligned with design/33:
// 10008 maps to HTTP 413 (request body too large); REQUEST_TIMEOUT is used by
// the Timeout middleware for HTTP 504, which has no dedicated 10xxx code yet.
const (
	codeBodyTooLarge = "10008"
	codeTimeout      = "REQUEST_TIMEOUT"
)

// traceIDPattern accepts only conservative trace ids: alphanumeric plus dash,
// 1-64 chars. Anything else (e.g. an attempted header injection) is treated
// as absent and replaced with a freshly generated UUID.
var traceIDPattern = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

// contextKey is an unexported key type so context values can never collide
// with keys from other packages.
type contextKey string

const traceIDKey contextKey = "trace_id"

// TraceIDToCtx stores the trace id in ctx so handlers and audit sinks can
// attach it to structured logs and error bodies (LLD 1.3.5).
func TraceIDToCtx(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

// TraceIDFromCtx returns the trace id previously stored via TraceIDToCtx, or
// "" when the middleware chain did not set one.
func TraceIDFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(traceIDKey).(string)
	return v
}

// newUUID generates a random UUIDv4 (RFC 4122) from crypto/rand only, so the
// package stays free of third-party dependencies. A broken CSPRNG is fatal:
// rand.Read panics by design, and propagating an error would be meaningless.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("httpx: crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// TraceID guarantees every request carries an X-Trace-ID (LLD 1.3.5,
// design/33 1.9): a client-supplied value is validated and reused, otherwise a
// random UUID is generated. The value is injected into the request header so
// downstream handlers observe it, echoed on the response header, and stored in
// the request context for logs and audits.
func TraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := strings.TrimSpace(r.Header.Get(HeaderTraceID))
		if !traceIDPattern.MatchString(traceID) {
			traceID = newUUID()
		}
		r.Header.Set(HeaderTraceID, traceID)
		w.Header().Set(HeaderTraceID, traceID)
		next.ServeHTTP(w, r.WithContext(TraceIDToCtx(r.Context(), traceID)))
	})
}

// BodyLimit caps the request body with http.MaxBytesReader (SEC-19). When a
// downstream handler reads past the limit, the middleware answers 413 with the
// unified error body (design/33 code 10008) as long as the handler has not
// committed a response yet. A non-positive limit disables the cap.
func BodyLimit(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			var exceeded atomic.Bool
			body := &limitedBody{
				ReadCloser: http.MaxBytesReader(w, r.Body, limit),
				exceeded:   &exceeded,
			}
			r.Body = body
			sw := &bodyLimitResponse{ResponseWriter: w, exceeded: &exceeded, traceID: TraceIDFrom(r)}
			next.ServeHTTP(sw, r)
			sw.finalize()
		})
	}
}

// limitedBody records when http.MaxBytesReader rejects an oversized read so
// the middleware can answer 413 with the unified error body.
type limitedBody struct {
	io.ReadCloser
	exceeded *atomic.Bool
}

// Read marks the body as exceeded when the wrapped reader surfaces an
// http.MaxBytesError; the error is returned unchanged to the handler.
func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			b.exceeded.Store(true)
		}
	}
	return n, err
}

// bodyLimitResponse rewrites the handler's response to 413 whenever the body
// limit was hit, unless the handler already committed a response.
type bodyLimitResponse struct {
	http.ResponseWriter
	exceeded *atomic.Bool
	traceID  string
	mu       sync.Mutex
	tookOver bool
}

func (w *bodyLimitResponse) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exceeded.Load() && !w.tookOver {
		w.tookOver = true
		WriteError(w.ResponseWriter, http.StatusRequestEntityTooLarge, codeBodyTooLarge,
			"request body too large", w.traceID)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *bodyLimitResponse) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exceeded.Load() && !w.tookOver {
		w.tookOver = true
		WriteError(w.ResponseWriter, http.StatusRequestEntityTooLarge, codeBodyTooLarge,
			"request body too large", w.traceID)
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// finalize covers handlers that hit the limit and returned without writing
// any response (e.g. they gave up on a decode error).
func (w *bodyLimitResponse) finalize() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exceeded.Load() && !w.tookOver {
		w.tookOver = true
		WriteError(w.ResponseWriter, http.StatusRequestEntityTooLarge, codeBodyTooLarge,
			"request body too large", w.traceID)
	}
}

// Timeout wraps next with http.TimeoutHandler so a stuck handler cannot hold
// the connection open forever (SEC-19, slowloris defense). The stdlib handler
// answers 503 on deadline expiry; this wrapper rewrites that response to 504
// with the unified error body, matching the timeout semantics of design/33
// (tool calls time out as 504). A non-positive duration disables the timeout
// and returns next unwrapped.
func Timeout(next http.Handler, d time.Duration) http.Handler {
	if d <= 0 {
		return next
	}
	var done atomic.Bool // set once the wrapped handler returns normally
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer done.Store(true)
		next.ServeHTTP(w, r)
	})
	th := http.TimeoutHandler(inner, d, "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &timeoutResponse{ResponseWriter: w, done: &done, traceID: TraceIDFrom(r)}
		th.ServeHTTP(sw, r)
		sw.finalize()
	})
}

// timeoutResponse intercepts the 503 written by http.TimeoutHandler on
// deadline expiry and replaces it with a 504 unified error body. The done
// flag distinguishes the timeout path (inner handler still running) from a
// legitimate 503 produced by the inner handler itself.
type timeoutResponse struct {
	http.ResponseWriter
	done     *atomic.Bool
	traceID  string
	mu       sync.Mutex
	tookOver bool
}

func (w *timeoutResponse) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if code == http.StatusServiceUnavailable && !w.done.Load() {
		w.tookOver = true
		WriteError(w.ResponseWriter, http.StatusGatewayTimeout, codeTimeout,
			"request timeout", w.traceID)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *timeoutResponse) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tookOver {
		return len(p), nil // discard http.TimeoutHandler's plain-text body
	}
	return w.ResponseWriter.Write(p)
}

// finalize covers the client-disconnect path (context canceled: the stdlib
// handler writes nothing) and any race where the deadline fired without a 503
// landing on this writer. If the inner handler never completed and no response
// was committed, answer 504 directly.
func (w *timeoutResponse) finalize() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done.Load() && !w.tookOver {
		w.tookOver = true
		WriteError(w.ResponseWriter, http.StatusGatewayTimeout, codeTimeout,
			"request timeout", w.traceID)
	}
}
