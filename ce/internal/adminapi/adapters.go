package adminapi

import (
	"context"
	"net/http"
	"slices"

	"adc.dev/ce/internal/adapters"
	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

// AdapterRegistry is the seam for the protocol-adapter registry (design/83
// C1.5); nil fails the handler closed.
type AdapterRegistry interface {
	AdaptersStatus(ctx context.Context) []adapters.AdapterStatus
}

// handleAdaptersList serves GET /v1/admin/adapters: the protocol adapter
// registry with per-adapter health for the console page (C1.5).
func (s *Server) handleAdaptersList(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if !slices.Contains(p.Roles, "platform_admin") && !slices.Contains(p.Roles, "tenant_admin") {
		writeError(w, r, http.StatusForbidden, codeForbidden, "forbidden")
		return
	}
	if s.Adapters == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "adapter registry not wired")
		return
	}
	statuses := s.Adapters.AdaptersStatus(r.Context())
	items := make([]map[string]any, 0, len(statuses))
	for _, st := range statuses {
		items = append(items, map[string]any{
			"protocol": st.Protocol,
			"version":  st.Version,
			"detail":   st.Detail,
			"healthy":  st.Healthy,
			"error":    st.Err,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
