// Class-A native MCP device binding endpoints (FR-021, design/82 B5.1,
// doc/07 chapter 2). The V1.5-reserved binding surface
// (design/33 2: "/v1/bindings series (FR-021)") is realized here under
// the admin prefix, following the existing device route convention:
//
//	POST /v1/admin/devices/{deviceID}/mcp-binding           initiate pairing
//	POST /v1/admin/devices/{deviceID}/mcp-binding/complete  consume pairing token + OAuth bind
//	POST /v1/admin/devices/{deviceID}/mcp-binding/revoke    unbind
//	GET  /v1/admin/devices/{deviceID}/mcp-binding           status query
//
// The pairing token (binding_token) is returned exactly once at
// initiation and stored only as a hash (NFR-004, doc/07 step 4); the
// OAuth client secret travels in the initiate request, is KEK-encrypted
// at rest and never leaves the service again.

package adminapi

import (
	"errors"
	"net/http"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
	"adc.dev/ce/internal/mcpbinding"
)

// initiateBindingRequest mirrors the doc/07 step 1-2 control-plane
// payload: the device MCP endpoint plus the platform's OAuth client
// credentials issued by the device-side authorization server.
type initiateBindingRequest struct {
	McpEndpoint  string `json:"mcp_endpoint"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	ChangeReason string `json:"change_reason"`
}

// initiateBindingResponse returns the one-shot pairing token (NFR-004).
type initiateBindingResponse struct {
	DeviceID       string    `json:"device_id"`
	Status         string    `json:"status"`
	McpEndpoint    string    `json:"mcp_endpoint"`
	BindingToken   string    `json:"binding_token"`
	BindingExpires time.Time `json:"binding_expire_at"`
}

type completeBindingRequest struct {
	BindingToken string `json:"binding_token"`
}

// completeBindingResponse reports the binding outcome. ToolsSynced is
// false with SyncError set when the OAuth binding succeeded but the
// catalog sync failed (binding stays BOUND; doc/07 step 5 degradation).
type completeBindingResponse struct {
	DeviceID    string `json:"device_id"`
	Status      string `json:"status"`
	ToolsSynced bool   `json:"tools_synced"`
	ToolsCount  int    `json:"tools_count"`
	SyncError   string `json:"sync_error,omitempty"`
}

type revokeBindingRequest struct {
	ChangeReason string `json:"change_reason"`
}

// bindingStatusResponse never carries tokens or secrets.
type bindingStatusResponse struct {
	DeviceID           string     `json:"device_id"`
	Status             string     `json:"status"`
	McpEndpoint        string     `json:"mcp_endpoint"`
	OAuthClientID      string     `json:"oauth_client_id"`
	TokenEndpoint      string     `json:"token_endpoint"`
	ResourceIdentifier string     `json:"resource_identifier"`
	BindingExpireAt    *time.Time `json:"binding_expire_at,omitempty"`
	BoundAt            *time.Time `json:"bound_at,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func bindingStatusView(b *mcpbinding.Binding) bindingStatusResponse {
	return bindingStatusResponse{
		DeviceID:           b.DeviceID,
		Status:             string(b.Status),
		McpEndpoint:        b.McpEndpoint,
		OAuthClientID:      b.OAuthClientID,
		TokenEndpoint:      b.TokenEndpoint,
		ResourceIdentifier: b.ResourceIdentifier,
		BindingExpireAt:    b.BindingExpireAt,
		BoundAt:            b.BoundAt,
		RevokedAt:          b.RevokedAt,
		UpdatedAt:          b.UpdatedAt.UTC(),
	}
}

// mapBindingError translates the mcpbinding sentinels onto the design/33
// code segments. FR-021 endpoints are V1.5-reserved in design/33, so the
// mapping reuses the 10xxx/11xxx segments: not found 11001, state
// conflict 10005, invalid/consumed pairing token 401 10002, OAuth
// failures 502 10007 (the device AS is a remote dependency).
func mapBindingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, mcpbinding.ErrBindingNotFound):
		writeError(w, r, http.StatusNotFound, codeDeviceNotFound, "device not found")
	case errors.Is(err, mcpbinding.ErrNotClassA):
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "only class A devices support mcp binding")
	case errors.Is(err, mcpbinding.ErrStateConflict):
		writeError(w, r, http.StatusConflict, codeConflict, "binding state does not allow this operation")
	case errors.Is(err, mcpbinding.ErrTokenInvalid), errors.Is(err, mcpbinding.ErrTokenExpired):
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "binding token invalid, expired or already consumed")
	case errors.Is(err, mcpbinding.ErrEndpointInvalid):
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "mcp_endpoint must be an absolute http(s) url")
	case errors.Is(err, mcpbinding.ErrOAuthFailed):
		writeError(w, r, http.StatusBadGateway, codeInternal, "device oauth binding failed")
	default:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

// loadOwnedBinding loads the binding and enforces tenant ownership
// (design/33 1.2, SEC-02) before any mutation.
func (s *Server) loadOwnedBinding(w http.ResponseWriter, r *http.Request, p *adminauth.Principal, deviceID string) (*mcpbinding.Binding, bool) {
	if s.Bindings == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "mcp binding not configured")
		return nil, false
	}
	b, err := s.Bindings.Get(r.Context(), deviceID)
	if err != nil {
		mapBindingError(w, r, err)
		return nil, false
	}
	if !s.checkOwnership(w, r, p, b.TenantID) {
		return nil, false
	}
	return b, true
}

// handleInitiateBinding serves POST /v1/admin/devices/{deviceID}/mcp-binding:
// registers the MCP endpoint and issues the one-shot pairing token
// (doc/07 step 1-2 control plane, status -> BINDING).
func (s *Server) handleInitiateBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	var req initiateBindingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ChangeReason == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	if req.ClientID == "" || req.ClientSecret == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "client_id and client_secret are required")
		return
	}
	if _, ok := s.loadOwnedBinding(w, r, p, deviceID); !ok {
		return
	}
	b, token, err := s.Bindings.Initiate(r.Context(), deviceID, mcpbinding.InitiateRequest{
		McpEndpoint:  req.McpEndpoint,
		ClientID:     req.ClientID,
		ClientSecret: req.ClientSecret,
	})
	if err != nil {
		mapBindingError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: b.TenantID,
		ActorID:  p.UserID,
		Action:   "device.mcp_binding.initiate",
		Target:   deviceID,
		Reason:   req.ChangeReason,
		Details:  map[string]any{"mcp_endpoint": req.McpEndpoint},
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, initiateBindingResponse{
		DeviceID:       b.DeviceID,
		Status:         string(b.Status),
		McpEndpoint:    b.McpEndpoint,
		BindingToken:   token,
		BindingExpires: b.BindingExpireAt.UTC(),
	})
}

// handleCompleteBinding serves POST /v1/admin/devices/{deviceID}/mcp-binding/complete:
// consumes the pairing token once, binds via OAuth 2.1 client
// credentials and syncs the tool catalog (doc/07 steps 3-5,
// status -> BOUND).
func (s *Server) handleCompleteBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	var req completeBindingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.BindingToken == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "binding_token is required")
		return
	}
	if _, ok := s.loadOwnedBinding(w, r, p, deviceID); !ok {
		return
	}
	b, n, syncErr := s.Bindings.Complete(r.Context(), deviceID, req.BindingToken)
	if b == nil {
		// Complete failed before the BOUND transition; the pairing
		// token was not consumed, so classify the underlying error.
		mapBindingError(w, r, syncErr)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: b.TenantID,
		ActorID:  p.UserID,
		Action:   "device.mcp_binding.complete",
		Target:   deviceID,
		TraceID:  httpx.TraceIDFrom(r),
	})
	resp := completeBindingResponse{
		DeviceID:    b.DeviceID,
		Status:      string(b.Status),
		ToolsSynced: syncErr == nil,
		ToolsCount:  n,
	}
	if syncErr != nil {
		resp.SyncError = syncErr.Error()
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// handleRevokeBinding serves POST /v1/admin/devices/{deviceID}/mcp-binding/revoke:
// unbinds the device (doc/07 step 7, status -> REVOKED).
func (s *Server) handleRevokeBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	var req revokeBindingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ChangeReason == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	if _, ok := s.loadOwnedBinding(w, r, p, deviceID); !ok {
		return
	}
	b, err := s.Bindings.Revoke(r.Context(), deviceID)
	if err != nil {
		mapBindingError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: b.TenantID,
		ActorID:  p.UserID,
		Action:   "device.mcp_binding.revoke",
		Target:   deviceID,
		Reason:   req.ChangeReason,
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, bindingStatusView(b))
}

// handleGetBinding serves GET /v1/admin/devices/{deviceID}/mcp-binding:
// the binding status query, never exposing tokens or secrets.
func (s *Server) handleGetBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	b, ok := s.loadOwnedBinding(w, r, p, deviceID)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bindingStatusView(b))
}
