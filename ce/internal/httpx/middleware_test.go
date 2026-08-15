package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var body ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not unified JSON: %v; raw=%q", err, rec.Body.String())
	}
	return body
}

func TestNewUUIDFormatAndUniqueness(t *testing.T) {
	a := newUUID()
	b := newUUID()
	if !uuidPattern.MatchString(a) {
		t.Fatalf("newUUID() = %q, not a UUIDv4", a)
	}
	if a == b {
		t.Fatalf("two successive UUIDs are identical: %q", a)
	}
}

func TestTimeoutExceededReturns504(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := Timeout(next, 20*time.Millisecond)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != codeTimeout {
		t.Fatalf("error code = %q, want %q", body.Code, codeTimeout)
	}
}

func TestTimeoutPassthrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	h := Timeout(next, time.Second)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

func TestTimeoutDisabledPassesThrough(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := Timeout(next, 0)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (timeout disabled)", rec.Code)
	}
}

func TestBodyLimitExceededReturns413(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	h := BodyLimit(16)(next)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(strings.Repeat("a", 32)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != codeBodyTooLarge {
		t.Fatalf("error code = %q, want %q", body.Code, codeBodyTooLarge)
	}
}

func TestBodyLimitExceededHandlerWritesNothing(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		// Give up silently; the middleware must still answer 413.
	})
	h := BodyLimit(16)(next)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(strings.Repeat("a", 32)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != codeBodyTooLarge {
		t.Fatalf("error code = %q, want %q", body.Code, codeBodyTooLarge)
	}
}

func TestBodyLimitWithinLimitPassesThrough(t *testing.T) {
	payload := strings.Repeat("a", 8)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("unexpected read error: %v", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(got)
	})
	h := BodyLimit(16)(next)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body = %q, want %q", rec.Body.String(), payload)
	}
}

func TestBodyLimitDisabled(t *testing.T) {
	payload := strings.Repeat("a", 32)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("unexpected read error: %v", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(got)
	})
	h := BodyLimit(0)(next)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (limit disabled)", rec.Code)
	}
	if rec.Body.String() != payload {
		t.Fatalf("body = %q, want %q", rec.Body.String(), payload)
	}
}

func TestTraceIDGeneratesWhenAbsent(t *testing.T) {
	var ctxID, hdrID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = TraceIDFromCtx(r.Context())
		hdrID = r.Header.Get(HeaderTraceID)
		w.WriteHeader(http.StatusOK)
	})
	h := TraceID(next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	respID := rec.Header().Get(HeaderTraceID)
	if !uuidPattern.MatchString(respID) {
		t.Fatalf("response X-Trace-ID = %q, not a UUIDv4", respID)
	}
	if ctxID != respID || hdrID != respID {
		t.Fatalf("mismatch: ctx=%q header=%q response=%q", ctxID, hdrID, respID)
	}
	if TraceIDFrom(req) != respID {
		t.Fatalf("TraceIDFrom(req) = %q, want %q", TraceIDFrom(req), respID)
	}
}

func TestTraceIDPassesThroughProvidedValue(t *testing.T) {
	provided := "trace-abc-123"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(HeaderTraceID); got != provided {
			t.Errorf("downstream header = %q, want %q", got, provided)
		}
		if got := TraceIDFromCtx(r.Context()); got != provided {
			t.Errorf("context trace id = %q, want %q", got, provided)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := TraceID(next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderTraceID, provided)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderTraceID); got != provided {
		t.Fatalf("response X-Trace-ID = %q, want %q", got, provided)
	}
}

func TestTraceIDRejectsUnsafeHeaderValue(t *testing.T) {
	unsafe := "evil\r\nSet-Cookie: hacked=1"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(HeaderTraceID); !uuidPattern.MatchString(got) {
			t.Errorf("downstream header = %q, want freshly generated UUID", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	h := TraceID(next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderTraceID, unsafe)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderTraceID); !uuidPattern.MatchString(got) {
		t.Fatalf("response X-Trace-ID = %q, want freshly generated UUID", got)
	}
}

func TestTraceIDIntoContext(t *testing.T) {
	ctx := TraceIDToCtx(context.Background(), "ctx-id")
	if got := TraceIDFromCtx(ctx); got != "ctx-id" {
		t.Fatalf("TraceIDFromCtx = %q, want %q", got, "ctx-id")
	}
	if got := TraceIDFromCtx(context.Background()); got != "" {
		t.Fatalf("TraceIDFromCtx(empty) = %q, want \"\"", got)
	}
}
