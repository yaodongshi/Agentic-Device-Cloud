package observe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMiddlewareCountsTenRequests is the design/80 B-09 acceptance test:
// ten requests through the middleware must land as counter value 10 with
// the correct route/code label shape.
func TestMiddlewareCountsTenRequests(t *testing.T) {
	s := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(s.HTTPRequests, s.HTTPRequestDuration, nil)(mux)

	srv := httptest.NewServer(h)
	defer srv.Close()
	for i := 0; i < 10; i++ {
		resp, err := http.Get(srv.URL + "/test")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	if got := s.HTTPRequests.Get("GET /test", "200"); got != 10 {
		t.Errorf("counter = %d, want 10", got)
	}
	body := scrape(t, s.Handler)
	if !strings.Contains(body, `adc_http_requests_total{route="GET /test",code="200"} 10`) {
		t.Errorf("counter line missing\n---\n%s", body)
	}
	if !strings.Contains(body, `adc_http_request_duration_seconds_count{route="GET /test",code="200"} 10`) {
		t.Errorf("histogram count line missing\n---\n%s", body)
	}
}

// TestMiddlewareRecordsErrorCode verifies non-200 responses are labeled with
// their actual status code.
func TestMiddlewareRecordsErrorCode(t *testing.T) {
	s := New()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /err", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	h := Middleware(s.HTTPRequests, s.HTTPRequestDuration, nil)(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/err", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := s.HTTPRequests.Get("POST /err", "500"); got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}

// TestMiddlewareCustomRouteResolver verifies routeFrom wins over the pattern.
func TestMiddlewareCustomRouteResolver(t *testing.T) {
	s := New()
	routeFrom := func(*http.Request) string { return "custom-route" }
	h := Middleware(s.HTTPRequests, s.HTTPRequestDuration, routeFrom)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))
	if got := s.HTTPRequests.Get("custom-route", "204"); got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}

// TestMiddlewareNilMetricsPassesThrough covers the nil-seam contract: with
// nil vecs the middleware must not disturb the response at all.
func TestMiddlewareNilMetricsPassesThrough(t *testing.T) {
	h := Middleware(nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Marker", "kept")
		w.WriteHeader(http.StatusCreated)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if rec.Header().Get("X-Marker") != "kept" {
		t.Fatalf("response headers disturbed by nil middleware")
	}
}
