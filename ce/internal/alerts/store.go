package alerts

import (
	"context"
	"sync"
	"time"
)

// RuleStore persists tenant-scoped alert rules (design/82 B2). List serves
// the Admin API for one tenant; All sweeps every tenant for the evaluator;
// Replace swaps the whole tenant rule set atomically (PUT full-replace
// semantics, same as the approval policy).
type RuleStore interface {
	List(ctx context.Context, tenantID string) ([]Rule, error)
	Replace(ctx context.Context, tenantID string, rules []Rule) error
	All(ctx context.Context) ([]Rule, error)
}

// MemoryRuleStore is the in-process RuleStore used by tests and by
// assemblies without PostgreSQL. It never validates tenant existence (it
// cannot); the PG store does.
type MemoryRuleStore struct {
	mu   sync.Mutex
	now  func() time.Time
	byID map[string][]Rule // tenantID -> rules, kept in insertion order
}

// NewMemoryRuleStore builds an empty in-memory rule store.
func NewMemoryRuleStore() *MemoryRuleStore {
	return &MemoryRuleStore{now: time.Now, byID: map[string][]Rule{}}
}

// List returns the stored rules of one tenant in insertion order.
func (m *MemoryRuleStore) List(_ context.Context, tenantID string) ([]Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneRules(m.byID[tenantID]), nil
}

// Replace swaps the tenant rule set. Times are stamped on entry; a rule
// with an empty ID gets one assigned (the PUT path may omit ids).
func (m *MemoryRuleStore) Replace(_ context.Context, tenantID string, rules []Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.ID == "" {
			r.ID = NewRuleID()
		}
		if r.CreatedAt.IsZero() {
			r.CreatedAt = now
		}
		r.UpdatedAt = now
		r.TenantID = tenantID
		out = append(out, r)
	}
	if len(out) == 0 {
		delete(m.byID, tenantID)
		return nil
	}
	m.byID[tenantID] = out
	return nil
}

// All returns every tenant's rules for the evaluator sweep.
func (m *MemoryRuleStore) All(_ context.Context) ([]Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Rule
	for _, rules := range m.byID {
		out = append(out, cloneRules(rules)...)
	}
	return out, nil
}

// cloneRules deep-copies a rule slice so callers cannot mutate the store
// through the returned aliases (labels map is the only reference field).
func cloneRules(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	for i, r := range rules {
		cp := r
		if r.Labels != nil {
			cp.Labels = make(map[string]string, len(r.Labels))
			for k, v := range r.Labels {
				cp.Labels[k] = v
			}
		}
		out[i] = cp
	}
	return out
}
