package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/ce/internal/adminauth"
)

// ---------------------------------------------------------------------------
// In-memory repository implementations. They mirror the PostgreSQL
// repository semantics (sentinels, quota CAS, tenant gates, soft delete)
// so handler tests run without a database.
// ---------------------------------------------------------------------------

// fixedNow is the test clock: deterministic timestamps for every fixture.
var fixedNow = time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)

// testKEK is a fixed 32-byte device credential KEK for hmac tests.
var testKEK = bytes.Repeat([]byte("k"), 32)

func uuidOf(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

type memStore struct {
	tenants    *memTenantRepo
	devices    *memDeviceRepo
	keys       *memApiKeyRepo
	tools      *memToolRepo
	auditLogs  *memAuditQueryRepo
	policies   *memPolicyRepo
	groups     *memGroupRepo
	importJobs *memImportJobRepo
}

func newMemStore() *memStore {
	s := &memStore{}
	s.tenants = newMemTenantRepo()
	s.devices = newMemDeviceRepo(s.tenants)
	s.keys = newMemApiKeyRepo(s.tenants)
	s.tools = newMemToolRepo(s.devices)
	s.auditLogs = newMemAuditQueryRepo()
	s.policies = newMemPolicyRepo(s.tenants)
	s.groups = newMemGroupRepo(s.tenants, s.devices)
	s.importJobs = newMemImportJobRepo()
	s.tenants.deviceCount = s.devices.count
	s.tenants.keyCount = s.keys.count
	now := func() time.Time { return fixedNow }
	s.tenants.now = now
	s.devices.now = now
	s.keys.now = now
	s.tools.now = now
	s.auditLogs.now = now
	s.policies.now = now
	s.groups.now = now
	s.importJobs.now = now
	return s
}

// --- tenants ---

type memTenantRepo struct {
	mu          sync.Mutex
	tenants     map[string]*Tenant
	byCode      map[string]string
	meta        map[string]map[string]any
	deviceCount func(tenantID string) int
	keyCount    func(tenantID string) int
	nextID      int
	now         func() time.Time
}

// --- budget usage (C5.1) ---

// memBudgetUsageRepo is the in-memory BudgetUsageRepo for handler tests:
// the current-month fee per tenant is seeded by tests and answers 0
// otherwise.
type memBudgetUsageRepo struct {
	mu  sync.Mutex
	fee map[string]int64
}

func newMemBudgetUsageRepo() *memBudgetUsageRepo {
	return &memBudgetUsageRepo{fee: map[string]int64{}}
}

func (m *memBudgetUsageRepo) setFee(tenantID string, fen int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fee[tenantID] = fen
}

func (m *memBudgetUsageRepo) CurrentMonthFeeFen(_ context.Context, tenantID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fee[tenantID], nil
}

func newMemTenantRepo() *memTenantRepo {
	return &memTenantRepo{tenants: map[string]*Tenant{}, byCode: map[string]string{}, meta: map[string]map[string]any{}, now: time.Now}
}

func cloneTenant(t *Tenant) *Tenant {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

func cloneMeta(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// GetMeta mirrors pgTenantRepo.GetMeta: nil for a tenant without a
// metadata document, ErrTenantNotFound for an unknown tenant.
func (r *memTenantRepo) GetMeta(ctx context.Context, tenantID string) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tenants[tenantID]; !ok {
		return nil, ErrTenantNotFound
	}
	return cloneMeta(r.meta[tenantID]), nil
}

// SetMeta merges keys into the tenant metadata document (nil value
// removes the key) and returns the merged document.
func (r *memTenantRepo) SetMeta(ctx context.Context, tenantID string, kv map[string]any) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tenants[tenantID]; !ok {
		return nil, ErrTenantNotFound
	}
	doc := r.meta[tenantID]
	if doc == nil {
		doc = map[string]any{}
	}
	for k, v := range kv {
		if v == nil {
			delete(doc, k)
			continue
		}
		doc[k] = v
	}
	if len(doc) == 0 {
		doc = nil
	}
	r.meta[tenantID] = doc
	return cloneMeta(doc), nil
}

func (r *memTenantRepo) Create(ctx context.Context, t *Tenant) (*Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byCode[t.Code]; exists {
		return nil, ErrTenantCodeConflict
	}
	r.nextID++
	now := r.now().UTC()
	cp := cloneTenant(t)
	cp.ID = uuidOf(r.nextID)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = now
	}
	r.tenants[cp.ID] = cp
	r.byCode[cp.Code] = cp.ID
	return cloneTenant(cp), nil
}

func (r *memTenantRepo) Get(ctx context.Context, tenantID string) (*Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[tenantID]
	if !ok {
		return nil, ErrTenantNotFound
	}
	return cloneTenant(t), nil
}

func (r *memTenantRepo) Update(ctx context.Context, tenantID string, t *Tenant) (*Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.tenants[tenantID]
	if !ok {
		return nil, ErrTenantNotFound
	}
	if t.Quota.MaxDevices < cur.UsedDevices || t.Quota.MonthlyCallLimit < cur.UsedCallsMonth {
		return nil, ErrTenantQuotaBelowUsage
	}
	cur.Quota = t.Quota
	cur.Status = t.Status
	cur.UpdatedAt = r.now().UTC()
	return cloneTenant(cur), nil
}

func (r *memTenantRepo) List(ctx context.Context, f TenantFilter, p Page) ([]Tenant, int, error) {
	r.mu.Lock()
	var candidates []*Tenant
	for _, t := range r.tenants {
		if f.TenantID != "" && t.ID != f.TenantID {
			continue
		}
		if f.Status != "" && t.Status != f.Status {
			continue
		}
		if f.Keyword != "" && !strings.Contains(strings.ToLower(t.Name), strings.ToLower(f.Keyword)) {
			continue
		}
		candidates = append(candidates, cloneTenant(t))
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})
	r.mu.Unlock()

	total := len(candidates)
	start := p.offset()
	if start > len(candidates) {
		start = len(candidates)
	}
	end := start + p.Size
	if end > len(candidates) {
		end = len(candidates)
	}
	out := make([]Tenant, 0, end-start)
	for _, t := range candidates[start:end] {
		if r.deviceCount != nil {
			t.DeviceCount = r.deviceCount(t.ID)
		}
		if r.keyCount != nil {
			t.AgentKeyCount = r.keyCount(t.ID)
		}
		out = append(out, *t)
	}
	return out, total, nil
}

// consumeDevice mirrors the conditional quota UPDATE of design/32 6.2.
func (r *memTenantRepo) consumeDevice(tenantID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[tenantID]
	if !ok {
		return ErrTenantNotFound
	}
	if t.Status != tenantStatusActive {
		return ErrTenantSuspended
	}
	if t.UsedDevices >= t.Quota.MaxDevices {
		return ErrDeviceQuotaExceeded
	}
	t.UsedDevices++
	return nil
}

func (r *memTenantRepo) releaseDevice(tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tenants[tenantID]; ok && t.UsedDevices > 0 {
		t.UsedDevices--
	}
}

// --- devices ---

type memDeviceRecord struct {
	d   Device
	del bool
}

type memDeviceRepo struct {
	mu      sync.Mutex
	devices map[string]*memDeviceRecord
	byCode  map[string]string
	tenants *memTenantRepo
	nextID  int
	now     func() time.Time
}

func newMemDeviceRepo(tenants *memTenantRepo) *memDeviceRepo {
	return &memDeviceRepo{
		devices: map[string]*memDeviceRecord{},
		byCode:  map[string]string{},
		tenants: tenants,
		now:     time.Now,
	}
}

func cloneDevice(d *Device) *Device {
	if d == nil {
		return nil
	}
	cp := *d
	if d.Metadata != nil {
		cp.Metadata = make(map[string]any, len(d.Metadata))
		for k, v := range d.Metadata {
			cp.Metadata[k] = v
		}
	}
	if d.GroupID != nil {
		g := *d.GroupID
		cp.GroupID = &g
	}
	if d.LastHeartbeat != nil {
		hb := *d.LastHeartbeat
		cp.LastHeartbeat = &hb
	}
	return &cp
}

func (r *memDeviceRepo) Register(ctx context.Context, d *Device, cred DeviceCredential) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byCode[d.DeviceCode]; exists {
		return nil, ErrDeviceCodeExists
	}
	if err := r.tenants.consumeDevice(d.TenantID); err != nil {
		return nil, err
	}
	r.nextID++
	now := r.now().UTC()
	cp := cloneDevice(d)
	cp.ID = uuidOf(100000 + r.nextID)
	cp.Status = deviceStatusOffline
	cp.CredentialVersion = 1
	cp.credentialStored = cred.Stored
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = now
	}
	r.devices[cp.ID] = &memDeviceRecord{d: *cp}
	r.byCode[cp.DeviceCode] = cp.ID
	return cloneDevice(cp), nil
}

func (r *memDeviceRepo) Get(ctx context.Context, deviceID string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.devices[deviceID]
	if !ok || rec.del {
		return nil, ErrDeviceNotFound
	}
	return cloneDevice(&rec.d), nil
}

func (r *memDeviceRepo) GetByCode(ctx context.Context, deviceCode string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byCode[deviceCode]
	if !ok {
		return nil, ErrDeviceNotFound
	}
	// byCode keeps burned codes forever (matches uq_devices_code).
	return cloneDevice(&r.devices[id].d), nil
}

func (r *memDeviceRepo) SetGroup(ctx context.Context, deviceID string, groupID *string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.lockGet(deviceID)
	if !ok {
		return nil, ErrDeviceNotFound
	}
	if groupID == nil {
		rec.d.GroupID = nil
	} else {
		g := *groupID
		rec.d.GroupID = &g
	}
	rec.d.UpdatedAt = r.now().UTC()
	return cloneDevice(&rec.d), nil
}

// detachGroup clears the group of every device (group delete, design/32
// 3.4 "删组时设备自动脱组"). Called with the group lock held; takes the
// device lock itself (devices never acquire the group lock, so the order
// is deadlock-free).
func (r *memDeviceRepo) detachGroup(groupID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.devices {
		if !rec.del && rec.d.GroupID != nil && *rec.d.GroupID == groupID {
			rec.d.GroupID = nil
		}
	}
}

func (r *memDeviceRepo) List(ctx context.Context, tenantID string, f DeviceFilter, p Page) ([]Device, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []*memDeviceRecord
	for _, rec := range r.devices {
		if rec.del || rec.d.TenantID != tenantID {
			continue
		}
		if f.Status != "" && rec.d.Status != f.Status {
			continue
		}
		if f.DeviceType != "" && rec.d.DeviceType != f.DeviceType {
			continue
		}
		if f.GroupID != "" && (rec.d.GroupID == nil || *rec.d.GroupID != f.GroupID) {
			continue
		}
		if f.Keyword != "" {
			kw := strings.ToLower(f.Keyword)
			if !strings.Contains(strings.ToLower(rec.d.DeviceCode), kw) &&
				!strings.Contains(strings.ToLower(rec.d.Name), kw) {
				continue
			}
		}
		all = append(all, rec)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].d.CreatedAt.Equal(all[j].d.CreatedAt) {
			return all[i].d.ID < all[j].d.ID
		}
		return all[i].d.CreatedAt.After(all[j].d.CreatedAt)
	})
	total := len(all)
	start := p.offset()
	if start > len(all) {
		start = len(all)
	}
	end := start + p.Size
	if end > len(all) {
		end = len(all)
	}
	out := make([]Device, 0, end-start)
	for _, rec := range all[start:end] {
		out = append(out, *cloneDevice(&rec.d))
	}
	return out, total, nil
}

func (r *memDeviceRepo) count(tenantID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.devices {
		if !rec.del && rec.d.TenantID == tenantID {
			n++
		}
	}
	return n
}

func (r *memDeviceRepo) lockGet(deviceID string) (*memDeviceRecord, bool) {
	rec, ok := r.devices[deviceID]
	if !ok || rec.del {
		return nil, false
	}
	return rec, true
}

func (r *memDeviceRepo) SetStatus(ctx context.Context, deviceID, status string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.lockGet(deviceID)
	if !ok {
		return nil, ErrDeviceNotFound
	}
	rec.d.Status = status
	rec.d.UpdatedAt = r.now().UTC()
	return cloneDevice(&rec.d), nil
}

func (r *memDeviceRepo) ResetCredential(ctx context.Context, deviceID string, cred DeviceCredential) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.lockGet(deviceID)
	if !ok {
		return nil, ErrDeviceNotFound
	}
	if rec.d.Status == deviceStatusFrozen {
		return nil, ErrDeviceFrozen
	}
	rec.d.credentialStored = cred.Stored
	rec.d.CredentialVersion++
	rec.d.UpdatedAt = r.now().UTC()
	return cloneDevice(&rec.d), nil
}

func (r *memDeviceRepo) RevokeCredential(ctx context.Context, deviceID string) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.lockGet(deviceID)
	if !ok {
		return nil, ErrDeviceNotFound
	}
	rec.d.credentialStored = "revoked$" + r.now().UTC().Format(time.RFC3339)
	rec.d.CredentialVersion++
	rec.d.UpdatedAt = r.now().UTC()
	return cloneDevice(&rec.d), nil
}

func (r *memDeviceRepo) UpdateMeta(ctx context.Context, deviceID string, name *string, metadata map[string]any) (*Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.lockGet(deviceID)
	if !ok {
		return nil, ErrDeviceNotFound
	}
	if name != nil {
		rec.d.Name = *name
	}
	if metadata != nil {
		rec.d.Metadata = metadata
	}
	rec.d.UpdatedAt = r.now().UTC()
	return cloneDevice(&rec.d), nil
}

func (r *memDeviceRepo) SoftDelete(ctx context.Context, deviceID string) error {
	r.mu.Lock()
	rec, ok := r.lockGet(deviceID)
	if !ok {
		r.mu.Unlock()
		return ErrDeviceNotFound
	}
	rec.del = true
	rec.d.Status = deviceStatusRetired
	tenantID := rec.d.TenantID
	r.mu.Unlock()
	r.tenants.releaseDevice(tenantID)
	return nil
}

// --- agent api keys ---

type memKeyRecord struct {
	k          ApiKey
	keyHash    string
	secretHash string
}

type memApiKeyRepo struct {
	mu      sync.Mutex
	keys    map[string]*memKeyRecord
	byHash  map[string]string
	tenants *memTenantRepo
	nextID  int
	now     func() time.Time
}

func newMemApiKeyRepo(tenants *memTenantRepo) *memApiKeyRepo {
	return &memApiKeyRepo{
		keys:    map[string]*memKeyRecord{},
		byHash:  map[string]string{},
		tenants: tenants,
		now:     time.Now,
	}
}

func cloneApiKey(k *ApiKey) *ApiKey {
	if k == nil {
		return nil
	}
	cp := *k
	cp.Scopes = append([]string(nil), k.Scopes...)
	if k.ExpiresAt != nil {
		t := *k.ExpiresAt
		cp.ExpiresAt = &t
	}
	if k.RevokedAt != nil {
		t := *k.RevokedAt
		cp.RevokedAt = &t
	}
	if k.LastUsedAt != nil {
		t := *k.LastUsedAt
		cp.LastUsedAt = &t
	}
	return &cp
}

func (r *memApiKeyRepo) tenantGate(tenantID string) error {
	t, err := r.tenants.Get(context.Background(), tenantID)
	if err != nil {
		return err
	}
	if t.Status != tenantStatusActive {
		return ErrTenantSuspended
	}
	return nil
}

func (r *memApiKeyRepo) newRecord(id string, k *NewKey) *memKeyRecord {
	now := r.now().UTC()
	return &memKeyRecord{
		k: ApiKey{
			ID:        id,
			TenantID:  k.TenantID,
			Name:      k.Name,
			AgentID:   k.AgentID,
			KeyPrefix: k.KeyPrefix,
			Scopes:    append([]string(nil), k.Scopes...),
			ScopeMode: "ALLOW_LIST",
			ExpiresAt: k.ExpiresAt,
			CreatedAt: now,
		},
		keyHash:    k.KeyHash,
		secretHash: k.SecretHash,
	}
}

func (r *memApiKeyRepo) Issue(ctx context.Context, k *NewKey) (*ApiKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.tenantGate(k.TenantID); err != nil {
		return nil, err
	}
	if _, exists := r.byHash[k.KeyHash]; exists {
		return nil, ErrApiKeyConflict
	}
	r.nextID++
	id := uuidOf(200000 + r.nextID)
	rec := r.newRecord(id, k)
	r.keys[id] = rec
	r.byHash[k.KeyHash] = id
	return cloneApiKey(&rec.k), nil
}

func (r *memApiKeyRepo) Get(ctx context.Context, keyID string) (*ApiKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.keys[keyID]
	if !ok {
		return nil, ErrApiKeyNotFound
	}
	return cloneApiKey(&rec.k), nil
}

func (r *memApiKeyRepo) List(ctx context.Context, tenantID string, p Page) ([]ApiKey, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []*memKeyRecord
	for _, rec := range r.keys {
		if rec.k.TenantID != tenantID {
			continue
		}
		all = append(all, rec)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].k.CreatedAt.Equal(all[j].k.CreatedAt) {
			return all[i].k.ID < all[j].k.ID
		}
		return all[i].k.CreatedAt.After(all[j].k.CreatedAt)
	})
	total := len(all)
	start := p.offset()
	if start > len(all) {
		start = len(all)
	}
	end := start + p.Size
	if end > len(all) {
		end = len(all)
	}
	out := make([]ApiKey, 0, end-start)
	for _, rec := range all[start:end] {
		out = append(out, *cloneApiKey(&rec.k))
	}
	return out, total, nil
}

func (r *memApiKeyRepo) count(tenantID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.keys {
		if rec.k.TenantID == tenantID && rec.k.RevokedAt == nil {
			n++
		}
	}
	return n
}

func (r *memApiKeyRepo) Revoke(ctx context.Context, keyID, reason string) (*ApiKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.keys[keyID]
	if !ok {
		return nil, ErrApiKeyNotFound
	}
	if rec.k.RevokedAt != nil {
		return nil, ErrApiKeyAlreadyRevoked
	}
	now := r.now().UTC()
	rec.k.RevokedAt = &now
	return cloneApiKey(&rec.k), nil
}

func (r *memApiKeyRepo) Rotate(ctx context.Context, keyID string, k *NewKey, reason string) (*ApiKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.keys[keyID]
	if !ok {
		return nil, ErrApiKeyNotFound
	}
	if old.k.RevokedAt != nil {
		return nil, ErrApiKeyAlreadyRevoked
	}
	if _, exists := r.byHash[k.KeyHash]; exists {
		return nil, ErrApiKeyConflict
	}
	now := r.now().UTC()
	old.k.RevokedAt = &now
	r.nextID++
	id := uuidOf(200000 + r.nextID)
	rec := r.newRecord(id, k)
	r.keys[id] = rec
	r.byHash[k.KeyHash] = id
	return cloneApiKey(&rec.k), nil
}

// --- device groups ---

type memGroupRepo struct {
	mu      sync.Mutex
	groups  map[string]*DeviceGroup
	tenants *memTenantRepo
	devices *memDeviceRepo
	nextID  int
	now     func() time.Time
}

func newMemGroupRepo(tenants *memTenantRepo, devices *memDeviceRepo) *memGroupRepo {
	return &memGroupRepo{
		groups:  map[string]*DeviceGroup{},
		tenants: tenants,
		devices: devices,
		now:     time.Now,
	}
}

func cloneGroup(g *DeviceGroup) *DeviceGroup {
	if g == nil {
		return nil
	}
	cp := *g
	if g.ParentID != nil {
		p := *g.ParentID
		cp.ParentID = &p
	}
	return &cp
}

func (r *memGroupRepo) tenantGate(tenantID string) error {
	t, err := r.tenants.Get(context.Background(), tenantID)
	if err != nil {
		return err
	}
	if t.Status != tenantStatusActive {
		return ErrTenantSuspended
	}
	return nil
}

func (r *memGroupRepo) Create(ctx context.Context, g *DeviceGroup) (*DeviceGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.tenantGate(g.TenantID); err != nil {
		return nil, err
	}
	for _, cur := range r.groups {
		if cur.TenantID == g.TenantID && cur.Name == g.Name {
			return nil, ErrGroupNameExists
		}
	}
	r.nextID++
	now := r.now().UTC()
	cp := cloneGroup(g)
	cp.ID = uuidOf(300000 + r.nextID)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = now
	}
	r.groups[cp.ID] = cp
	return cloneGroup(cp), nil
}

func (r *memGroupRepo) Get(ctx context.Context, groupID string) (*DeviceGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groups[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	return cloneGroup(g), nil
}

func (r *memGroupRepo) List(ctx context.Context, tenantID string, p Page) ([]DeviceGroup, int, error) {
	r.mu.Lock()
	var all []*DeviceGroup
	for _, g := range r.groups {
		if g.TenantID == tenantID {
			all = append(all, cloneGroup(g))
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID < all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	r.mu.Unlock()
	total := len(all)
	start := p.offset()
	if start > len(all) {
		start = len(all)
	}
	end := start + p.Size
	if end > len(all) {
		end = len(all)
	}
	out := make([]DeviceGroup, 0, end-start)
	for _, g := range all[start:end] {
		out = append(out, *g)
	}
	return out, total, nil
}

func (r *memGroupRepo) Update(ctx context.Context, groupID string, name, description *string) (*DeviceGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groups[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	if name != nil {
		for _, cur := range r.groups {
			if cur.ID != groupID && cur.TenantID == g.TenantID && cur.Name == *name {
				return nil, ErrGroupNameExists
			}
		}
		g.Name = *name
	}
	if description != nil {
		g.Description = *description
	}
	g.UpdatedAt = r.now().UTC()
	return cloneGroup(g), nil
}

func (r *memGroupRepo) Delete(ctx context.Context, groupID string) error {
	r.mu.Lock()
	if _, ok := r.groups[groupID]; !ok {
		r.mu.Unlock()
		return ErrGroupNotFound
	}
	delete(r.groups, groupID)
	r.devices.detachGroup(groupID)
	r.mu.Unlock()
	return nil
}

// --- import jobs ---

type memImportJobRepo struct {
	mu   sync.Mutex
	jobs map[string]*ImportJob
	now  func() time.Time
}

func newMemImportJobRepo() *memImportJobRepo {
	return &memImportJobRepo{jobs: map[string]*ImportJob{}, now: time.Now}
}

func cloneImportJob(j *ImportJob) *ImportJob {
	if j == nil {
		return nil
	}
	cp := *j
	cp.Errors = append([]ImportRowError(nil), j.Errors...)
	if j.FinishedAt != nil {
		f := *j.FinishedAt
		cp.FinishedAt = &f
	}
	return &cp
}

func (r *memImportJobRepo) Create(ctx context.Context, j *ImportJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.TaskID] = cloneImportJob(j)
	return nil
}

func (r *memImportJobRepo) Update(ctx context.Context, j *ImportJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.TaskID] = cloneImportJob(j)
	return nil
}

func (r *memImportJobRepo) Get(ctx context.Context, jobID string) (*ImportJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[jobID]
	if !ok {
		return nil, ErrImportJobNotFound
	}
	return cloneImportJob(j), nil
}

// --- session store / audit sink / test harness ---

type memSessions struct {
	mu      sync.Mutex
	m       map[string]*adminauth.Session
	current map[string]*adminauth.AuthorizationSnapshot
}

func (s *memSessions) Create(ctx context.Context, sess *adminauth.Session, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*adminauth.Session{}
	}
	cp := *sess
	cp.Roles = append([]string(nil), sess.Roles...)
	s.m[sess.TokenHash] = &cp
	if s.current == nil {
		s.current = map[string]*adminauth.AuthorizationSnapshot{}
	}
	s.current[sess.UserID+"\x00"+sess.TenantID] = &adminauth.AuthorizationSnapshot{UserStatus: "ACTIVE", TenantStatus: "ACTIVE", AuthzVersion: sess.AuthzVersion, Roles: append([]string(nil), sess.Roles...)}
	return nil
}

func (s *memSessions) Get(ctx context.Context, tokenHash string) (*adminauth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[tokenHash]
	if !ok {
		return nil, adminauth.ErrSessionNotFound
	}
	cp := *v
	cp.Roles = append([]string(nil), v.Roles...)
	return &cp, nil
}

func (s *memSessions) Delete(ctx context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tokenHash)
	return nil
}

func (s *memSessions) CurrentAuthorization(ctx context.Context, userID, tenantID string) (*adminauth.AuthorizationSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.current[userID+"\x00"+tenantID]
	if snapshot != nil {
		copy := *snapshot
		copy.Roles = append([]string(nil), snapshot.Roles...)
		return &copy, nil
	}
	return nil, adminauth.ErrAuthorizationInvalid
}

type memAudit struct {
	mu  sync.Mutex
	ops []AdminOp
}

func (a *memAudit) Record(ctx context.Context, op AdminOp) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ops = append(a.ops, op)
	return nil
}

func (a *memAudit) last() (AdminOp, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ops) == 0 {
		return AdminOp{}, false
	}
	return a.ops[len(a.ops)-1], true
}

type testEnv struct {
	store    *memStore
	sessions *memSessions
	audit    *memAudit
	budget   *memBudgetUsageRepo
	srv      *Server
	handler  http.Handler
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st := newMemStore()
	sess := &memSessions{m: map[string]*adminauth.Session{}, current: map[string]*adminauth.AuthorizationSnapshot{}}
	aud := &memAudit{}
	srv := NewServer(st.tenants, st.devices, st.keys, sess)
	srv.Audit = aud
	srv.Tools = st.tools
	srv.AuditQuery = st.auditLogs
	srv.Policies = st.policies
	srv.ImportJobs = st.importJobs
	srv.Groups = st.groups
	srv.BudgetUsage = newMemBudgetUsageRepo()
	srv.KEK = testKEK
	srv.Now = func() time.Time { return fixedNow }
	env := &testEnv{store: st, sessions: sess, audit: aud, srv: srv, handler: srv.Handler()}
	env.budget = srv.BudgetUsage.(*memBudgetUsageRepo)
	return env
}

// token issues a session for the given roles and tenant, returning the
// bearer token.
func (e *testEnv) token(t *testing.T, roles []string, tenantID string) string {
	t.Helper()
	tok, err := adminauth.NewToken()
	if err != nil {
		t.Fatalf("adminauth.NewToken: %v", err)
	}
	userNumber := 900099
	if slices.Contains(roles, rolePlatformAdmin) {
		userNumber = 900000
	} else if slices.Contains(roles, "tenant_admin") {
		userNumber = 900000
	} else if slices.Contains(roles, "approver") {
		userNumber = 900002
	} else if slices.Contains(roles, "auditor") {
		userNumber = 900003
	}
	userID := uuidOf(userNumber)
	if e.sessions.current == nil {
		e.sessions.current = map[string]*adminauth.AuthorizationSnapshot{}
	}
	e.sessions.m[adminauth.HashToken(tok)] = &adminauth.Session{
		TokenHash:    adminauth.HashToken(tok),
		UserID:       userID,
		TenantID:     tenantID,
		AuthzVersion: 1,
		Roles:        roles,
		ExpiresAt:    fixedNow.Add(time.Hour),
	}
	e.sessions.current[userID+"\x00"+tenantID] = &adminauth.AuthorizationSnapshot{UserStatus: "ACTIVE", TenantStatus: "ACTIVE", AuthzVersion: 1, Roles: append([]string(nil), roles...)}
	return tok
}

// do issues one request through the assembled handler.
func (e *testEnv) do(method, path, token string, body any) *httptest.ResponseRecorder {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			panic(fmt.Sprintf("marshal request body: %v", err))
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// assertError checks the HTTP status and the unified error code
// (design/33 1.5).
func (e *testEnv) assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, rec.Body.String())
	}
	var eb struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
		t.Fatalf("error body is not json: %v; body: %s", err, rec.Body.String())
	}
	if eb.Code != code {
		t.Fatalf("code = %q, want %q; body: %s", eb.Code, code, rec.Body.String())
	}
}

func (e *testEnv) decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
}

// createTenant is the platform_admin fixture for creating a tenant.
func (e *testEnv) createTenant(t *testing.T, code, name string, quota *quotaRequest) tenantResponse {
	t.Helper()
	tok := e.token(t, []string{rolePlatformAdmin}, "")
	rec := e.do(http.MethodPost, "/v1/admin/tenants", tok, createTenantRequest{Name: name, Code: code, Quota: quota})
	if rec.Code != http.StatusCreated {
		t.Fatalf("createTenant %q: status = %d; body: %s", code, rec.Code, rec.Body.String())
	}
	var out tenantResponse
	e.decode(t, rec, &out)
	return out
}
