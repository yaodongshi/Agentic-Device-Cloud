package agentauth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"adc.dev/ce/internal/httpx"
)

// HeaderXADCKey carries the agent key token (design/33 1.2):
// "X-ADC-Key: adc_<keyID>_<secret>".
const HeaderXADCKey = "X-ADC-Key"

// design/33 1.5 business error codes used by this middleware.
const (
	codeUnauthorized = "10002" // unauthenticated or invalid credential
	codeInternal     = "10007" // service internal error
)

const (
	msgUnauthorized = "invalid or expired api key"
	msgInternal     = "internal error"
)

type principalCtxKey struct{}

// WithPrincipal attaches the authenticated principal to ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// FromContext returns the principal injected by Authorize, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok
}

// Authorize returns middleware that requires a valid Agent API key. The
// token is read from X-ADC-Key or Authorization: Bearer (design/33 1.2);
// on success the Principal is injected into the request context for the
// downstream handler. Any tenant declared in request headers is ignored
// (SEC-02 / GAP-13): tenant context comes only from the key record.
func Authorize(validator ApiKeyValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)
			if token == "" {
				writeAuthError(w, r)
				return
			}
			principal, err := validator.Validate(r.Context(), token)
			if err != nil {
				if isAuthError(err) {
					writeAuthError(w, r)
				} else {
					httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// writeAuthError emits a generic 401 so that the failure reason is not
// revealed to callers (no enumeration of valid key ids).
func writeAuthError(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusUnauthorized, codeUnauthorized, msgUnauthorized, httpx.TraceIDFrom(r))
}

// isAuthError reports whether err is one of the four sentinel validation
// failures; transport and storage errors are not auth errors.
func isAuthError(err error) bool {
	return errors.Is(err, ErrKeyNotFound) ||
		errors.Is(err, ErrKeyDisabled) ||
		errors.Is(err, ErrKeyExpired) ||
		errors.Is(err, ErrBadSecret)
}

// extractToken returns the agent key token from X-ADC-Key or
// Authorization: Bearer. X-ADC-Key wins when both are present. Any other
// Authorization scheme is ignored.
func extractToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderXADCKey)); v != "" {
		return v
	}
	const bearerScheme = "Bearer "
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > len(bearerScheme) && strings.EqualFold(auth[:len(bearerScheme)], bearerScheme) {
		return strings.TrimSpace(auth[len(bearerScheme):])
	}
	return ""
}
