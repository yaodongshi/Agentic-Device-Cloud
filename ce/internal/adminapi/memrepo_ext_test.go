package adminapi

// In-memory repository implementations for the B-04/B-06 seams (device
// tools, approval policy, audit query). They mirror the PostgreSQL
// semantics the handlers rely on: sentinels, deterministic sort order,
// keyset pagination and the export row cap, so handler tests run without
// a database.

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- device tools ---

type memToolRepo struct {
	mu      sync.Mutex
	tools   map[string]*DeviceTool // key: deviceID + "\x00" + toolName
	devices *memDeviceRepo
	nextID  int
	now     func() time.Time
}

func newMemToolRepo(devices *memDeviceRepo) *memToolRepo {
	return &memToolRepo{tools: map[string]*DeviceTool{}, devices: devices, now: time.Now}
}

func toolKey(deviceID, toolName string) string { return deviceID + "\x00" + toolName }

func cloneTool(t *DeviceTool) *DeviceTool {
	if t == nil {
		return nil
	}
	cp := *t
	if t.InputSchema != nil {
		cp.InputSchema = make(map[string]any, len(t.InputSchema))
		for k, v := range t.InputSchema {
			cp.InputSchema[k] = v
		}
	}
	return &cp
}

// seedTool inserts a tool row directly (the data plane tools/list report
// is out of scope for these tests).
func (r *memToolRepo) seedTool(deviceID, tenantID, name string, risk int, enabled bool, desc string) *DeviceTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	t := &DeviceTool{
		ID:            uuidOf(300000 + r.nextID),
		TenantID:      tenantID,
		DeviceID:      deviceID,
		ToolName:      name,
		Description:   desc,
		InputSchema:   map[string]any{"type": "object"},
		RiskLevel:     risk,
		SchemaVersion: "1.0",
		IsEnabled:     enabled,
		UpdatedAt:     r.now().UTC(),
	}
	r.tools[toolKey(deviceID, name)] = t
	return cloneTool(t)
}

// get is the unlocked accessor shared by tests.
func (r *memToolRepo) get(deviceID, toolName string) (*DeviceTool, bool) {
	t, ok := r.tools[toolKey(deviceID, toolName)]
	if !ok {
		return nil, false
	}
	return cloneTool(t), true
}

func (r *memToolRepo) ListTools(ctx context.Context, deviceID string, p Page) ([]DeviceTool, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []*DeviceTool
	for _, t := range r.tools {
		if t.DeviceID == deviceID {
			all = append(all, t)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ToolName < all[j].ToolName })
	total := len(all)
	start := p.offset()
	if start > len(all) {
		start = len(all)
	}
	end := start + p.Size
	if end > len(all) {
		end = len(all)
	}
	out := make([]DeviceTool, 0, end-start)
	for _, t := range all[start:end] {
		out = append(out, *cloneTool(t))
	}
	return out, total, nil
}

func (r *memToolRepo) GetTool(ctx context.Context, deviceID, toolName string) (*DeviceTool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.get(deviceID, toolName)
	if !ok {
		return nil, ErrToolNotFound
	}
	return t, nil
}

func (r *memToolRepo) UpdateTools(ctx context.Context, deviceID string, changes []ToolChange, changedBy string) ([]ToolUpdateOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ToolUpdateOutcome, 0, len(changes))
	for _, c := range changes {
		t, ok := r.get(deviceID, c.ToolName)
		if !ok {
			out = append(out, ToolUpdateOutcome{ToolName: c.ToolName, Err: ErrToolNotFound})
			continue
		}
		if c.RiskLevel != nil {
			t.RiskLevel = *c.RiskLevel
			t.RiskChangedBy = changedBy // design/32 3.6: FR-006 change must be attributable
		}
		if c.IsEnabled != nil {
			t.IsEnabled = *c.IsEnabled
		}
		t.UpdatedAt = r.now().UTC()
		r.tools[toolKey(deviceID, c.ToolName)] = t
		out = append(out, ToolUpdateOutcome{ToolName: c.ToolName, Tool: cloneTool(t)})
	}
	return out, nil
}

// --- approval policy ---

type memPolicyRepo struct {
	mu       sync.Mutex
	policies map[string]*ApprovalPolicy
	tenants  *memTenantRepo
	now      func() time.Time
}

func newMemPolicyRepo(tenants *memTenantRepo) *memPolicyRepo {
	return &memPolicyRepo{policies: map[string]*ApprovalPolicy{}, tenants: tenants, now: time.Now}
}

func clonePolicy(p *ApprovalPolicy) *ApprovalPolicy {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Approvers = append([]string{}, p.Approvers...)
	return &cp
}

func (r *memPolicyRepo) GetPolicy(ctx context.Context, tenantID string) (*ApprovalPolicy, error) {
	if _, err := r.tenants.Get(ctx, tenantID); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.policies[tenantID]; ok {
		return clonePolicy(p), nil
	}
	return defaultPolicy(tenantID), nil
}

// SetPolicy swaps the stored policy (BR-007-03: only the tenant-level
// source of truth changes; snapshots already taken by ticket creation
// keep their values, so in-flight tickets are unaffected). The stored
// copy owns its slices so later mutation of the caller's struct cannot
// leak into the ledger.
func (r *memPolicyRepo) SetPolicy(ctx context.Context, tenantID string, p *ApprovalPolicy) (*ApprovalPolicy, error) {
	if _, err := r.tenants.Get(ctx, tenantID); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := clonePolicy(p)
	stored.UpdatedAt = r.now().UTC()
	r.policies[tenantID] = stored
	return clonePolicy(stored), nil
}

// --- audit logs ---

type memAuditQueryRepo struct {
	mu   sync.Mutex
	logs []AuditLog
	now  func() time.Time
}

func newMemAuditQueryRepo() *memAuditQueryRepo {
	return &memAuditQueryRepo{now: time.Now}
}

// seed appends one audit row directly (the audit write path via
// JetStream is out of scope for these tests).
func (r *memAuditQueryRepo) seed(l AuditLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.CreatedAt.IsZero() {
		l.CreatedAt = r.now().UTC()
	}
	r.logs = append(r.logs, l)
}

func cloneAuditLog(l AuditLog) AuditLog {
	cp := l
	if l.RiskLevel != nil {
		v := *l.RiskLevel
		cp.RiskLevel = &v
	}
	if l.ExecutionDurationMS != nil {
		v := *l.ExecutionDurationMS
		cp.ExecutionDurationMS = &v
	}
	cp.RequestParams = cloneJSONMap(l.RequestParams)
	cp.ResponsePayload = cloneJSONMap(l.ResponsePayload)
	return cp
}

func cloneJSONMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// matchAudit implements the filter semantics the PG repository must
// mirror with SQL predicates.
func matchAudit(l AuditLog, f AuditFilter) bool {
	if l.TenantID != f.TenantID {
		return false
	}
	if f.TimeFrom != nil && l.CreatedAt.Before(*f.TimeFrom) {
		return false
	}
	if f.TimeTo != nil && l.CreatedAt.After(*f.TimeTo) {
		return false
	}
	if f.DeviceID != "" && l.DeviceID != f.DeviceID {
		return false
	}
	if f.DeviceCode != "" && l.DeviceCode != f.DeviceCode {
		return false
	}
	if f.ToolName != "" && l.ToolName != f.ToolName {
		return false
	}
	if f.Status != "" && l.Status != f.Status {
		return false
	}
	if f.EventType != "" && l.EventType != f.EventType {
		return false
	}
	if f.AgentID != "" && l.ActorID != f.AgentID {
		return false
	}
	if f.Keyword != "" {
		kw := strings.ToLower(f.Keyword)
		hay := strings.ToLower(strings.Join([]string{l.ToolName, l.ActorID, l.HitlApprover, l.HitlComment}, "\x00"))
		if !strings.Contains(hay, kw) {
			return false
		}
	}
	return true
}

// sortedMatches returns the filtered rows in created_at DESC, id DESC
// order (design/33 1.6).
func sortedMatches(logs []AuditLog, f AuditFilter) []AuditLog {
	out := make([]AuditLog, 0, len(logs))
	for _, l := range logs {
		if matchAudit(l, f) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// isAfterCursor reports whether l sorts strictly after the cursor row in
// DESC (created_at, id) order, i.e. l is older than the cursor row.
func isAfterCursor(l AuditLog, c AuditCursor) bool {
	if l.CreatedAt.Before(c.CreatedAt) {
		return true
	}
	if l.CreatedAt.After(c.CreatedAt) {
		return false
	}
	return l.ID < c.ID
}

func (r *memAuditQueryRepo) Query(ctx context.Context, f AuditFilter, after *AuditCursor, limit int) (*AuditPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	matched := sortedMatches(r.logs, f)
	total := len(matched)
	start := 0
	if after != nil {
		// matched is DESC sorted, so the "older than cursor" predicate
		// transitions false -> true exactly once.
		start = sort.Search(len(matched), func(i int) bool { return isAfterCursor(matched[i], *after) })
	}
	end := start + limit
	if end > len(matched) {
		end = len(matched)
	}
	page := &AuditPage{Items: make([]AuditLog, 0, end-start), Total: total}
	for _, l := range matched[start:end] {
		page.Items = append(page.Items, cloneAuditLog(l))
	}
	if end < len(matched) {
		last := matched[end-1]
		page.NextCursor = &AuditCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (r *memAuditQueryRepo) Count(ctx context.Context, f AuditFilter) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(sortedMatches(r.logs, f)), nil
}

func (r *memAuditQueryRepo) Export(ctx context.Context, f AuditFilter, maxRows int, w io.Writer) (int, error) {
	r.mu.Lock()
	matched := sortedMatches(r.logs, f)
	r.mu.Unlock()
	if err := writeAuditCSV(w, [][]string{auditCSVHeader}); err != nil {
		return 0, err
	}
	n := 0
	for _, l := range matched {
		if n >= maxRows {
			return n, ErrAuditExportLimitExceeded
		}
		if err := writeAuditCSV(w, [][]string{auditCSVRow(cloneAuditLog(l))}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
