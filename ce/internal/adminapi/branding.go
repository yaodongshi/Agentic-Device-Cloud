package adminapi

// C2.1 white-label branding seam (design/83): the console login page and
// app chrome take their title / logo / primary color from the tenant
// branding stored in adc_tenants.metadata under the "branding" key:
//
//	{"branding": {"title": "Acme Console", "logo_url": "https://cdn/logo.png", "primary_color": "#1d4ed8"}}
//
// Endpoints:
//
//	GET /v1/admin/branding  public (no session): the login page needs the
//	                        brand before authentication. The optional
//	                        tenant_id query parameter resolves against the
//	                        tenant metadata; unknown tenants or a missing
//	                        key fall back to the platform default.
//	PUT /v1/admin/branding  platform_admin exclusive; partial update
//	                        (absent fields keep their values) merged into
//	                        the metadata JSONB like every other key.
//
// The handlers read/write through the TenantRepo metadata seam
// (GetMeta/SetMeta, tenants.go), so no dedicated repository exists: the
// merge discipline guarantees the sibling keys (approval_policy,
// alert_rules, quota fields, budget line) survive every branding write.

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

// brandingMetaKey is the adc_tenants.metadata JSONB key.
const brandingMetaKey = "branding"

// branding field sanity bounds (design/83 C2.1): the title must stay a
// short line in the app chrome, the logo URL a single well-formed
// http(s) reference. javascript:/data: schemes are rejected because the
// console renders the URL in an <img> (SEC-20).
const (
	maxBrandingTitleLen   = 255
	maxBrandingLogoURLLen = 2048
)

var brandingColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Branding is the tenant white-label configuration (design/83 C2.1).
type Branding struct {
	Title        string
	LogoURL      string
	PrimaryColor string
}

// defaultBranding is the platform default when no tenant branding is
// configured: it mirrors the console tokens.css --adc-brand token.
func defaultBranding() Branding {
	return Branding{Title: "ADC Console", PrimaryColor: "#2563eb"}
}

// brandFromMeta reads the branding document out of a raw metadata map;
// absent or malformed values fall back to the platform defaults. The
// second result reports whether a branding document existed at all.
func brandFromMeta(meta map[string]any) (Branding, bool) {
	b := defaultBranding()
	raw, ok := meta[brandingMetaKey]
	if !ok {
		return b, false
	}
	doc, ok := raw.(map[string]any)
	if !ok {
		return b, false
	}
	if v, ok := doc["title"].(string); ok && strings.TrimSpace(v) != "" {
		b.Title = strings.TrimSpace(v)
	}
	if v, ok := doc["logo_url"].(string); ok {
		b.LogoURL = strings.TrimSpace(v)
	}
	if v, ok := doc["primary_color"].(string); ok && brandingColorRe.MatchString(v) {
		b.PrimaryColor = v
	}
	return b, true
}

// brandingResponse is the wire shape shared by GET and PUT.
type brandingResponse struct {
	Title        string `json:"title"`
	LogoURL      string `json:"logo_url"`
	PrimaryColor string `json:"primary_color"`
	IsDefault    bool   `json:"is_default"`
}

func newBrandingResponse(b Branding, isDefault bool) brandingResponse {
	return brandingResponse{
		Title:        b.Title,
		LogoURL:      b.LogoURL,
		PrimaryColor: b.PrimaryColor,
		IsDefault:    isDefault,
	}
}

// handleGetBranding serves GET /v1/admin/branding (public, design/83
// C2.1). The optional tenant_id query parameter switches to the tenant's
// white label; anything unreadable (bad uuid, unknown tenant, missing
// key) degrades to the platform default instead of failing the login
// page.
func (s *Server) handleGetBranding(w http.ResponseWriter, r *http.Request) {
	b := defaultBranding()
	isDefault := true
	if tid := strings.TrimSpace(r.URL.Query().Get("tenant_id")); tid != "" && s.Tenants != nil {
		if isValidUUID(tid) {
			if meta, err := s.Tenants.GetMeta(r.Context(), tid); err == nil {
				if tb, ok := brandFromMeta(meta); ok {
					b, isDefault = tb, false
				}
			}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, newBrandingResponse(b, isDefault))
}

// putBrandingRequest is the PUT /v1/admin/branding body: a partial
// update (absent fields keep their values) plus the mandatory audit
// reason. An empty string on a pointer field clears/resets that field
// (logo removed, color back to the platform default); the title cannot
// be cleared.
type putBrandingRequest struct {
	TenantID     string  `json:"tenant_id"`
	Title        *string `json:"title"`
	LogoURL      *string `json:"logo_url"`
	PrimaryColor *string `json:"primary_color"`
	ChangeReason string  `json:"change_reason"`
}

// validateLogoURL accepts http(s) URLs only (the console renders the
// value in an <img>, SEC-20) and caps the length.
func validateLogoURL(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > maxBrandingLogoURLLen {
		return errors.New("logo_url is too long")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("logo_url must be an http(s) URL")
	}
	return nil
}

// handlePutBranding serves PUT /v1/admin/branding. White-labeling is a
// platform commercial feature (design/83 C2.1), so only platform_admin
// may write; the branding document is merged into the tenant metadata
// JSONB (policies.go discipline).
func (s *Server) handlePutBranding(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if !isPlatformAdmin(p) {
		writeError(w, r, http.StatusForbidden, codeForbidden, "branding updates require the platform_admin role")
		return
	}
	var req putBrandingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !requireUUID(w, r, "tenant_id", req.TenantID) {
		return
	}
	if strings.TrimSpace(req.ChangeReason) == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	current := defaultBranding()
	meta, err := s.Tenants.GetMeta(r.Context(), req.TenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load tenant metadata failed")
		return
	}
	if meta != nil {
		current, _ = brandFromMeta(meta)
	}
	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" || len(title) > maxBrandingTitleLen {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "title must be 1-255 chars")
			return
		}
		current.Title = title
	}
	if req.LogoURL != nil {
		logo := strings.TrimSpace(*req.LogoURL)
		if err := validateLogoURL(logo); err != nil {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		current.LogoURL = logo
	}
	if req.PrimaryColor != nil {
		color := strings.TrimSpace(*req.PrimaryColor)
		if color == "" {
			color = defaultBranding().PrimaryColor
		}
		if !brandingColorRe.MatchString(color) {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "primary_color must be a #RRGGBB hex color")
			return
		}
		current.PrimaryColor = color
	}
	if _, err := s.Tenants.SetMeta(r.Context(), req.TenantID, map[string]any{
		brandingMetaKey: map[string]any{
			"title":         current.Title,
			"logo_url":      current.LogoURL,
			"primary_color": current.PrimaryColor,
		},
	}); err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "update branding failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: req.TenantID,
		ActorID:  p.UserID,
		Action:   "branding.update",
		Target:   req.TenantID,
		Reason:   req.ChangeReason,
		Details: map[string]any{
			"title":         current.Title,
			"logo_url":      current.LogoURL,
			"primary_color": current.PrimaryColor,
		},
		TraceID: httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, newBrandingResponse(current, false))
}
