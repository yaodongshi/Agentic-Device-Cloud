package connector

import (
	"sync"
)

// DeviceHub is the local routing table, authoritative for this node
// (design/31 3.1.7, GAP-11). Keys are (tenantID, deviceCode) pairs; each entry
// carries a generation counter used by the kick-old mechanism (SEC-15).
type DeviceHub struct {
	mu      sync.RWMutex
	tenants map[string]map[string]*hubEntry // tenantID -> deviceCode -> entry
}

type hubEntry struct {
	gen     uint64
	session *DeviceSession
}

// NewDeviceHub creates an empty hub.
func NewDeviceHub() *DeviceHub {
	return &DeviceHub{
		tenants: make(map[string]map[string]*hubEntry),
	}
}

// Register inserts the session under (TenantID, DeviceID) and assigns it the
// next generation for that key. If a previous session exists it is returned
// as `old` (the caller kicks it: kick notification + Close). The previous
// entry is replaced immediately, so GetDevice always resolves to the newest
// generation; the old session's late onClose will fail the gen check in
// UnregisterIfCurrent and must not remove the new entry (SEC-15).
//
// Register never blocks on network I/O: closing the old session is the
// caller's job, outside the hub lock.
func (h *DeviceHub) Register(s *DeviceSession) (old *DeviceSession) {
	h.mu.Lock()
	defer h.mu.Unlock()

	byDevice, ok := h.tenants[s.TenantID]
	if !ok {
		byDevice = make(map[string]*hubEntry)
		h.tenants[s.TenantID] = byDevice
	}

	var nextGen uint64
	if prev, exists := byDevice[s.DeviceID]; exists {
		nextGen = prev.gen + 1
		old = prev.session
	}

	s.gen = nextGen
	byDevice[s.DeviceID] = &hubEntry{gen: s.gen, session: s}
	return old
}

// UnregisterIfCurrent removes the entry only if it still belongs to the given
// generation and reports whether it did. A stale session (kicked or replaced)
// carries an older gen and therefore cannot delete the newer session's route
// (SEC-15); the caller must skip derived-state cleanup (registry unregister,
// offline event) when false, or the old session would tear down the new one's
// routing index.
func (h *DeviceHub) UnregisterIfCurrent(tenantID, deviceCode string, gen uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	byDevice, ok := h.tenants[tenantID]
	if !ok {
		return false
	}
	entry, exists := byDevice[deviceCode]
	if !exists || entry.gen != gen {
		return false
	}
	delete(byDevice, deviceCode)
	if len(byDevice) == 0 {
		delete(h.tenants, tenantID)
	}
	return true
}

// GetDevice returns the session of the current generation only. A closed but
// not yet cleaned-up session is reported as offline.
func (h *DeviceHub) GetDevice(tenantID, deviceCode string) (*DeviceSession, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	byDevice, ok := h.tenants[tenantID]
	if !ok {
		return nil, ErrDeviceOffline
	}
	entry, exists := byDevice[deviceCode]
	if !exists || entry.session.IsClosed() {
		return nil, ErrDeviceOffline
	}
	return entry.session, nil
}

// CloseAll closes every live session (graceful shutdown path). Each session's
// read pump will exit and deliver its own onClose, which performs the
// gen-checked hub removal and registry cleanup.
func (h *DeviceHub) CloseAll() {
	h.mu.RLock()
	sessions := make([]*DeviceSession, 0, 8)
	for _, byDevice := range h.tenants {
		for _, entry := range byDevice {
			sessions = append(sessions, entry.session)
		}
	}
	h.mu.RUnlock()

	for _, s := range sessions {
		s.Close()
	}
}

// Size returns the number of registered sessions (test/metric helper).
func (h *DeviceHub) Size() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, byDevice := range h.tenants {
		n += len(byDevice)
	}
	return n
}
