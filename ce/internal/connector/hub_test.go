package connector

import (
	"errors"
	"sync"
	"testing"
)

// newMinSession builds a session without a live websocket connection: the hub
// only reads TenantID/DeviceID/gen/IsClosed, so no network is required here.
func newMinSession(id, tenant string) *DeviceSession {
	return NewDeviceSession(id, tenant, nil)
}

// TestHubRegisterBumpsGeneration verifies that each Register for the same
// device key receives a strictly increasing generation (SEC-15).
func TestHubRegisterBumpsGeneration(t *testing.T) {
	h := NewDeviceHub()
	s1 := newMinSession("d1", "t1")
	s2 := newMinSession("d1", "t1")
	s3 := newMinSession("d1", "t1")

	h.Register(s1)
	h.Register(s2)
	h.Register(s3)

	if s1.Gen() != 0 || s2.Gen() != 1 || s3.Gen() != 2 {
		t.Fatalf("generations = %d,%d,%d, want 0,1,2", s1.Gen(), s2.Gen(), s3.Gen())
	}
}

// TestHubRegisterReturnsOldSession verifies the kick-old contract: Register
// returns the previous session and immediately replaces the route.
func TestHubRegisterReturnsOldSession(t *testing.T) {
	h := NewDeviceHub()
	s1 := newMinSession("d1", "t1")
	s2 := newMinSession("d1", "t1")

	if old := h.Register(s1); old != nil {
		t.Fatalf("first register returned stale session %v", old)
	}
	if old := h.Register(s2); old != s1 {
		t.Fatalf("second register returned %v, want s1", old)
	}
	if got := h.Size(); got != 1 {
		t.Fatalf("hub size = %d, want 1", got)
	}
}

// TestHubGetDeviceReturnsCurrentGeneration verifies GetDevice only resolves
// the newest generation after a re-register (SEC-15).
func TestHubGetDeviceReturnsCurrentGeneration(t *testing.T) {
	h := NewDeviceHub()
	s1 := newMinSession("d1", "t1")
	h.Register(s1)

	s2 := newMinSession("d1", "t1")
	h.Register(s2)

	got, err := h.GetDevice("t1", "d1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if got != s2 {
		t.Fatalf("GetDevice returned stale session %p (gen %d), want %p (gen %d)",
			got, got.Gen(), s2, s2.Gen())
	}
}

// TestHubStaleUnregisterDoesNotRemoveNewEntry is the SEC-15 core scenario: the
// kicked old session's late onClose must not delete the new session's route.
func TestHubStaleUnregisterDoesNotRemoveNewEntry(t *testing.T) {
	h := NewDeviceHub()
	oldS := newMinSession("d1", "t1")
	h.Register(oldS)

	newS := newMinSession("d1", "t1")
	h.Register(newS)

	// The old session's read pump exits and runs its onClose with its stale
	// generation; the gen check must reject the removal and report it.
	if removed := h.UnregisterIfCurrent("t1", "d1", oldS.Gen()); removed {
		t.Fatal("stale unregister reported a removal")
	}

	if got := h.Size(); got != 1 {
		t.Fatalf("stale unregister removed the new route: size = %d, want 1", got)
	}
	got, err := h.GetDevice("t1", "d1")
	if err != nil {
		t.Fatalf("GetDevice after stale unregister: %v", err)
	}
	if got != newS {
		t.Fatalf("route now points to %p, want new session %p", got, newS)
	}
}

// TestHubCurrentUnregisterRemovesEntry verifies that the genuine cleanup
// (current generation) removes the route and cleans up empty tenants.
func TestHubCurrentUnregisterRemovesEntry(t *testing.T) {
	h := NewDeviceHub()
	s := newMinSession("d1", "t1")
	h.Register(s)

	if !h.UnregisterIfCurrent("t1", "d1", s.Gen()) {
		t.Fatal("current-generation unregister reported no removal")
	}
	// A second unregister of the same gen must be a no-op.
	if h.UnregisterIfCurrent("t1", "d1", s.Gen()) {
		t.Fatal("second unregister of the same gen reported a removal")
	}
	if got := h.Size(); got != 0 {
		t.Fatalf("size = %d, want 0", got)
	}
	if _, err := h.GetDevice("t1", "d1"); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("GetDevice err = %v, want ErrDeviceOffline", err)
	}
	if len(h.tenants) != 0 {
		t.Fatalf("empty tenant map not cleaned up")
	}
}

// TestHubGetDeviceOfflineCases verifies offline resolution for unknown keys
// and for a registered-but-closed session.
func TestHubGetDeviceOfflineCases(t *testing.T) {
	h := NewDeviceHub()

	if _, err := h.GetDevice("unknown", "d1"); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("unknown tenant err = %v, want ErrDeviceOffline", err)
	}

	s := newMinSession("d1", "t1")
	h.Register(s)
	if _, err := h.GetDevice("t1", "unknown"); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("unknown device err = %v, want ErrDeviceOffline", err)
	}

	s.Close()
	if _, err := h.GetDevice("t1", "d1"); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("closed session err = %v, want ErrDeviceOffline", err)
	}
}

// TestHubCloseAllClosesEverySession verifies the graceful-shutdown path.
func TestHubCloseAllClosesEverySession(t *testing.T) {
	h := NewDeviceHub()
	s1 := newMinSession("d1", "t1")
	s2 := newMinSession("d2", "t1")
	s3 := newMinSession("d3", "t2")
	h.Register(s1)
	h.Register(s2)
	h.Register(s3)

	if got := h.Size(); got != 3 {
		t.Fatalf("size = %d, want 3", got)
	}

	h.CloseAll()
	for i, s := range []*DeviceSession{s1, s2, s3} {
		if !s.IsClosed() {
			t.Fatalf("session %d not closed by CloseAll", i)
		}
	}
}

// TestHubConcurrentReRegister stresses the generation mechanism under race
// detection: N concurrent reconnects for one device must converge to exactly
// one current session, and only its unregister may remove the route.
func TestHubConcurrentReRegister(t *testing.T) {
	h := NewDeviceHub()
	const n = 16
	sessions := make([]*DeviceSession, n)
	for i := range sessions {
		sessions[i] = newMinSession("d1", "t1")
	}

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *DeviceSession) {
			defer wg.Done()
			h.Register(s)
		}(s)
	}
	wg.Wait()

	if got := h.Size(); got != 1 {
		t.Fatalf("size = %d, want 1", got)
	}
	cur, err := h.GetDevice("t1", "d1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}

	// All stale generations must not remove the route; only the current one
	// may.
	for _, s := range sessions {
		if s != cur {
			if h.UnregisterIfCurrent("t1", "d1", s.Gen()) {
				t.Fatal("stale unregister reported a removal")
			}
		}
	}
	if got := h.Size(); got != 1 {
		t.Fatalf("stale unregisters removed route: size = %d, want 1", got)
	}
	if !h.UnregisterIfCurrent("t1", "d1", cur.Gen()) {
		t.Fatal("current unregister reported no removal")
	}
	if got := h.Size(); got != 0 {
		t.Fatalf("size = %d, want 0", got)
	}
}

// TestHubSize verifies the size metric across tenants.
func TestHubSize(t *testing.T) {
	h := NewDeviceHub()
	if got := h.Size(); got != 0 {
		t.Fatalf("empty hub size = %d, want 0", got)
	}
	h.Register(newMinSession("d1", "t1"))
	h.Register(newMinSession("d2", "t1"))
	h.Register(newMinSession("d1", "t2"))
	if got := h.Size(); got != 3 {
		t.Fatalf("size = %d, want 3", got)
	}
}
