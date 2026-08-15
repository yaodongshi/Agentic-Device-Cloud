package adminapi

// design/80 B-04 first half: tool risk level configuration (FR-006).
// Endpoints:
//
//	GET   /v1/admin/devices/{deviceID}/tools  (design/33 3.1.10)
//	PATCH /v1/admin/devices/{deviceID}/tools  (design/33 3.1.11)
//
// Risk levels (0 read-only / 1 low / 2 high HITL / 3 critical physical
// loop, design/32 3.6) are the platform-authoritative interception
// source (SEC-09). Every risk change persists risk_changed_by = the
// acting admin (adc_users.id, SEC-21 real subject) and emits an
// admin_op audit event carrying before/after values plus the mandatory
// change_reason (design/31 3.4.4). Downgrading a tool out of the HITL
// band (level >= 2 -> <= 1) requires the X-ADC-Confirm: true header,
// the API-layer second confirmation of FR-006 / design/33 3.1.11
// (HBR-5).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

// ErrToolNotFound maps to 404 code 11005 (design/33 3.1.11).
var ErrToolNotFound = errors.New("adminapi: tool not found")

// confirmHeader is the downgrade second-confirmation header (design/33
// 3.1.11: X-ADC-Confirm: true).
const confirmHeader = "X-ADC-Confirm"

// maxToolChanges bounds one PATCH batch (one page of tools per device;
// keeps validation lookups and the audit trail bounded).
const maxToolChanges = 200

// validRiskLevel mirrors the adc_device_tools.risk_level CHECK 0-3
// (design/32 3.6).
func validRiskLevel(v int) bool { return v >= 0 && v <= 3 }

// isRiskDowngrade reports whether the change lowers a tool out of the
// HITL band (level >= 2) into the free-run band (level <= 1), the
// FR-006 exception path that forces second confirmation.
func isRiskDowngrade(before, after int) bool { return before >= 2 && after <= 1 }

// DeviceTool is one adc_device_tools row (design/32 3.6) in API
// spelling. RiskChangedBy is the adc_users.id of the last admin who
// changed the risk level (FR-006 audit).
type DeviceTool struct {
	ID            string
	TenantID      string
	DeviceID      string
	ToolName      string
	DisplayName   string
	Description   string
	InputSchema   map[string]any
	RiskLevel     int
	RiskChangedBy string
	SchemaVersion string
	IsEnabled     bool
	UpdatedAt     time.Time
}

// ToolChange is one PATCH batch entry (design/33 3.1.11 changes[]);
// nil fields are left untouched.
type ToolChange struct {
	ToolName  string
	RiskLevel *int
	IsEnabled *bool
}

// ToolUpdateOutcome is the per-change result of UpdateTools. Failures
// (e.g. ErrToolNotFound) ride inside the outcomes so one bad tool name
// does not abort the whole batch (design/33 partial-update semantics).
type ToolUpdateOutcome struct {
	ToolName string
	Tool     *DeviceTool
	Err      error
}

// ToolRepo persists device tool configuration (design/31 LLD 3.4.2
// toolpolicy seam).
type ToolRepo interface {
	ListTools(ctx context.Context, deviceID string, p Page) ([]DeviceTool, int, error)
	GetTool(ctx context.Context, deviceID, toolName string) (*DeviceTool, error)
	// UpdateTools applies changes in request order. The repository sets
	// risk_changed_by to changedBy on every risk_level mutation
	// (design/32 3.6).
	UpdateTools(ctx context.Context, deviceID string, changes []ToolChange, changedBy string) ([]ToolUpdateOutcome, error)
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// deviceToolItem mirrors design/33 3.1.10.
type deviceToolItem struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	InputSchema   map[string]any `json:"input_schema"`
	RiskLevel     int            `json:"risk_level"`
	IsEnabled     bool           `json:"is_enabled"`
	SchemaVersion string         `json:"schema_version"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type deviceToolListResponse struct {
	Items    []deviceToolItem `json:"items"`
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// toolChangeRequest mirrors design/33 3.1.11 changes[]; pointer fields
// distinguish absent from zero.
type toolChangeRequest struct {
	Name      string `json:"name"`
	RiskLevel *int   `json:"risk_level"`
	IsEnabled *bool  `json:"is_enabled"`
}

type patchToolsRequest struct {
	Changes      []toolChangeRequest `json:"changes"`
	ChangeReason string              `json:"change_reason"`
}

type toolChangeFailure struct {
	Name    string `json:"name"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type patchToolsResponse struct {
	Changed int                 `json:"changed"`
	Failed  []toolChangeFailure `json:"failed"`
}

// handleListDeviceTools serves GET /v1/admin/devices/{deviceID}/tools
// (design/33 3.1.10): the device tool ledger with platform-authoritative
// risk levels. Tool metadata arrives from the data plane tools/list
// reports; this endpoint only reads the ledger.
func (s *Server) handleListDeviceTools(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	dev, err := s.Devices.Get(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			writeError(w, r, http.StatusNotFound, codeDeviceNotFound, "device not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load device failed")
		return
	}
	if !s.checkOwnership(w, r, p, dev.TenantID) {
		return
	}
	if s.Tools == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "tool repository not wired")
		return
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	list, total, err := s.Tools.ListTools(r.Context(), deviceID, page)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list tools failed")
		return
	}
	items := make([]deviceToolItem, 0, len(list))
	for i := range list {
		items = append(items, deviceToolItem{
			Name:          list[i].ToolName,
			Description:   list[i].Description,
			InputSchema:   list[i].InputSchema,
			RiskLevel:     list[i].RiskLevel,
			IsEnabled:     list[i].IsEnabled,
			SchemaVersion: list[i].SchemaVersion,
			UpdatedAt:     list[i].UpdatedAt.UTC(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, deviceToolListResponse{Items: items, Total: total, Page: page.Number, PageSize: page.Size})
}

// handlePatchDeviceTools serves PATCH /v1/admin/devices/{deviceID}/tools
// (design/33 3.1.11): batch risk_level / is_enabled configuration.
//
// Contract enforcement:
//   - change_reason is mandatory on every batch (design/33 3.1.11).
//   - risk_level outside 0-3 is rejected with 10001.
//   - lowering a tool from >= 2 to <= 1 without X-ADC-Confirm: true is
//     rejected with 10001 (FR-006 second confirmation, HBR-5).
//   - unknown tool names land in failed[] with code 11005, the rest of
//     the batch still applies (partial update).
//
// Every successful risk change persists risk_changed_by and emits one
// admin_op audit event with before/after values and the reason
// (design/31 3.4.4: downgrade and upgrade get equal audit strength).
func (s *Server) handlePatchDeviceTools(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	dev, err := s.Devices.Get(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			writeError(w, r, http.StatusNotFound, codeDeviceNotFound, "device not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load device failed")
		return
	}
	if !s.checkOwnership(w, r, p, dev.TenantID) {
		return
	}
	if s.Tools == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "tool repository not wired")
		return
	}
	var req patchToolsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ChangeReason) == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	if len(req.Changes) == 0 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "changes must not be empty")
		return
	}
	if len(req.Changes) > maxToolChanges {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "too many changes in one batch")
		return
	}
	confirm := strings.EqualFold(strings.TrimSpace(r.Header.Get(confirmHeader)), "true")
	// before holds the current tool per change, captured once and reused
	// for the downgrade guard and the audit before-values.
	before := make(map[string]*DeviceTool, len(req.Changes))
	for i := range req.Changes {
		c := &req.Changes[i]
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" || len(c.Name) > 128 {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "changes[].name must be 1-128 chars")
			return
		}
		if c.RiskLevel == nil && c.IsEnabled == nil {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "each change must set risk_level or is_enabled")
			return
		}
		if c.RiskLevel != nil && !validRiskLevel(*c.RiskLevel) {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "risk_level must be between 0 and 3")
			return
		}
		cur, gerr := s.Tools.GetTool(r.Context(), deviceID, c.Name)
		if gerr != nil {
			if errors.Is(gerr, ErrToolNotFound) {
				continue // surfaces as a failed[] entry below
			}
			writeError(w, r, http.StatusInternalServerError, codeInternal, "load tool failed")
			return
		}
		before[c.Name] = cur
		if c.RiskLevel != nil && isRiskDowngrade(cur.RiskLevel, *c.RiskLevel) && !confirm {
			writeError(w, r, http.StatusBadRequest, codeBadRequest,
				"risk level downgrade requires second confirmation (X-ADC-Confirm: true)")
			return
		}
	}
	changes := make([]ToolChange, len(req.Changes))
	for i := range req.Changes {
		changes[i] = ToolChange{
			ToolName:  req.Changes[i].Name,
			RiskLevel: req.Changes[i].RiskLevel,
			IsEnabled: req.Changes[i].IsEnabled,
		}
	}
	outcomes, err := s.Tools.UpdateTools(r.Context(), deviceID, changes, p.UserID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "update tools failed")
		return
	}
	resp := patchToolsResponse{Changed: 0, Failed: []toolChangeFailure{}}
	for i := range outcomes {
		oc := &outcomes[i]
		if oc.Err != nil {
			if errors.Is(oc.Err, ErrToolNotFound) {
				resp.Failed = append(resp.Failed, toolChangeFailure{
					Name:    oc.ToolName,
					Code:    codeToolNotFound,
					Message: "tool not found",
				})
				continue
			}
			writeError(w, r, http.StatusInternalServerError, codeInternal, "update tools failed")
			return
		}
		resp.Changed++
		ch := changes[i]
		if b, okb := before[oc.ToolName]; okb && (ch.RiskLevel != nil || ch.IsEnabled != nil) {
			details := map[string]any{"tool_name": oc.ToolName}
			if ch.RiskLevel != nil {
				details["risk_before"] = b.RiskLevel
				details["risk_after"] = *ch.RiskLevel
			}
			if ch.IsEnabled != nil {
				details["is_enabled_before"] = b.IsEnabled
				details["is_enabled_after"] = *ch.IsEnabled
			}
			s.recordAudit(r.Context(), AdminOp{
				EventID:  newEventID(),
				TenantID: dev.TenantID,
				ActorID:  p.UserID,
				Action:   "tool.configure",
				Target:   deviceID,
				Reason:   req.ChangeReason,
				Details:  details,
				TraceID:  httpx.TraceIDFrom(r),
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
