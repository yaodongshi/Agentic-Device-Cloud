package adminapi

import (
	"net/http"
	"testing"
	"time"
)

func createGroup(t *testing.T, env *testEnv, tok string, body map[string]any) groupResponse {
	t.Helper()
	rec := env.do(http.MethodPost, "/v1/admin/device-groups", tok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create group = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out groupResponse
	env.decode(t, rec, &out)
	return out
}

func TestDeviceGroupsCRUD(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	// Create with description and parent (self as root of the tree).
	root := createGroup(t, env, tok, map[string]any{"name": "产线A", "description": "A1 车间"})
	if root.GroupID == "" || root.Name != "产线A" || root.Description != "A1 车间" {
		t.Fatalf("group = %+v", root)
	}
	child := createGroup(t, env, tok, map[string]any{"name": "产线A-子组", "parent_id": root.GroupID})
	if child.ParentID == nil || *child.ParentID != root.GroupID {
		t.Fatalf("child parent = %v, want %s", child.ParentID, root.GroupID)
	}

	// Duplicate name in the same tenant: 409 code 11014.
	env.assertError(t, env.do(http.MethodPost, "/v1/admin/device-groups", tok,
		map[string]any{"name": "产线A"}), http.StatusConflict, codeGroupNameExists)
	// Missing name: 400.
	env.assertError(t, env.do(http.MethodPost, "/v1/admin/device-groups", tok,
		map[string]any{"description": "x"}), http.StatusBadRequest, codeBadRequest)

	// List: two groups, newest first.
	rec := env.do(http.MethodGet, "/v1/admin/device-groups", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list groups = %d; body: %s", rec.Code, rec.Body.String())
	}
	var list groupListResponse
	env.decode(t, rec, &list)
	if list.Total != 2 || len(list.Items) != 2 {
		t.Fatalf("list = %+v, want total=2", list)
	}

	// Patch rename + description.
	rec = env.do(http.MethodPatch, "/v1/admin/device-groups/"+root.GroupID, tok,
		map[string]any{"name": "产线A-1", "description": "A1 车间（改）"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch group = %d; body: %s", rec.Code, rec.Body.String())
	}
	var patched groupResponse
	env.decode(t, rec, &patched)
	if patched.Name != "产线A-1" || patched.Description != "A1 车间（改）" {
		t.Fatalf("patched = %+v", patched)
	}
	// Patch with neither field: 400; rename to a taken name: 409.
	env.assertError(t, env.do(http.MethodPatch, "/v1/admin/device-groups/"+root.GroupID, tok,
		map[string]any{}), http.StatusBadRequest, codeBadRequest)
	env.assertError(t, env.do(http.MethodPatch, "/v1/admin/device-groups/"+root.GroupID, tok,
		map[string]any{"name": child.Name}), http.StatusConflict, codeGroupNameExists)

	// Delete child then root; second delete is 404.
	rec = env.do(http.MethodDelete, "/v1/admin/device-groups/"+child.GroupID, tok, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete child = %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodDelete, "/v1/admin/device-groups/"+child.GroupID, tok, nil)
	env.assertError(t, rec, http.StatusNotFound, codeGroupNotFound)
	rec = env.do(http.MethodDelete, "/v1/admin/device-groups/"+root.GroupID, tok, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete root = %d; body: %s", rec.Code, rec.Body.String())
	}
	// Audit trail carries create/patch/delete with the actor.
	op, ok := env.audit.last()
	if !ok || op.Action != "group.delete" || op.Target != root.GroupID {
		t.Fatalf("audit = %+v, want group.delete", op)
	}
}

func TestDeviceGroupsParentValidation(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	aTok := env.token(t, []string{"tenant_admin"}, a.TenantID)
	bTok := env.token(t, []string{"tenant_admin"}, b.TenantID)
	bGroup := createGroup(t, env, bTok, map[string]any{"name": "B 产线"})

	// Non-uuid parent: 400; unknown group: 404; cross-tenant group: 403.
	env.assertError(t, env.do(http.MethodPost, "/v1/admin/device-groups", aTok,
		map[string]any{"name": "x", "parent_id": "nope"}), http.StatusBadRequest, codeBadRequest)
	env.assertError(t, env.do(http.MethodPost, "/v1/admin/device-groups", aTok,
		map[string]any{"name": "x", "parent_id": uuidOf(77777)}), http.StatusNotFound, codeGroupNotFound)
	env.assertError(t, env.do(http.MethodPost, "/v1/admin/device-groups", aTok,
		map[string]any{"name": "x", "parent_id": bGroup.GroupID}), http.StatusForbidden, codeCrossTenant)
}

func TestDeviceGroupAssignmentAndDetach(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	grp := createGroup(t, env, tok, map[string]any{"name": "产线A"})

	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-g", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d; body: %s", rec.Code, rec.Body.String())
	}
	var reg deviceRegisterResponse
	env.decode(t, rec, &reg)

	// Attach via PATCH op=update_meta + group_id.
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "update_meta", "change_reason": "入组", "group_id": grp.GroupID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("attach = %d; body: %s", rec.Code, rec.Body.String())
	}
	env.store.devices.mu.Lock()
	attached := env.store.devices.devices[reg.DeviceID].d.GroupID
	env.store.devices.mu.Unlock()
	if attached == nil || *attached != grp.GroupID {
		t.Fatalf("device group = %v, want %s", attached, grp.GroupID)
	}
	// The device list filters by the group (design/33 3.1.7).
	rec = env.do(http.MethodGet, "/v1/admin/devices?group_id="+grp.GroupID, tok, nil)
	var list deviceListResponse
	env.decode(t, rec, &list)
	if list.Total != 1 || list.Items[0].DeviceCode != "dev-g" {
		t.Fatalf("group filter = %+v, want dev-g only", list)
	}

	// Detach via group_id="".
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "update_meta", "change_reason": "移组", "group_id": "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("detach = %d; body: %s", rec.Code, rec.Body.String())
	}
	env.store.devices.mu.Lock()
	detached := env.store.devices.devices[reg.DeviceID].d.GroupID
	env.store.devices.mu.Unlock()
	if detached != nil {
		t.Fatalf("device group = %v, want nil after detach", detached)
	}

	// Invalid uuid: 400; unknown group: 404.
	env.assertError(t, env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "update_meta", "change_reason": "x", "group_id": "nope",
	}), http.StatusBadRequest, codeBadRequest)
	env.assertError(t, env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "update_meta", "change_reason": "x", "group_id": uuidOf(77777),
	}), http.StatusNotFound, codeGroupNotFound)
}

func TestDeviceGroupDeleteDetachesDevices(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	grp := createGroup(t, env, tok, map[string]any{"name": "产线B"})

	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-h", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d; body: %s", rec.Code, rec.Body.String())
	}
	var reg deviceRegisterResponse
	env.decode(t, rec, &reg)
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID, tok, map[string]any{
		"op": "update_meta", "change_reason": "入组", "group_id": grp.GroupID,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("attach = %d; body: %s", rec.Code, rec.Body.String())
	}

	rec = env.do(http.MethodDelete, "/v1/admin/device-groups/"+grp.GroupID, tok, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete group = %d; body: %s", rec.Code, rec.Body.String())
	}
	// The device survived and its group was cleared (design/32 3.4).
	env.store.devices.mu.Lock()
	d := env.store.devices.devices[reg.DeviceID].d
	env.store.devices.mu.Unlock()
	if d.GroupID != nil || d.Status == deviceStatusRetired {
		t.Fatalf("device after group delete = %+v, want detached and alive", d)
	}
}

func TestDeviceGroupsRolesAndTenantIsolation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	createGroup(t, env, tok, map[string]any{"name": "产线C"})

	// Unauthenticated: 401.
	env.assertError(t, env.do(http.MethodGet, "/v1/admin/device-groups", "", nil),
		http.StatusUnauthorized, codeUnauthorized)
	// Auditor: 403 (admin-only surface).
	aud := env.token(t, []string{"auditor"}, tr.TenantID)
	env.assertError(t, env.do(http.MethodGet, "/v1/admin/device-groups", aud, nil),
		http.StatusForbidden, codeForbidden)
	// Platform admin must name the tenant.
	padmin := env.token(t, []string{rolePlatformAdmin}, "")
	env.assertError(t, env.do(http.MethodGet, "/v1/admin/device-groups", padmin, nil),
		http.StatusBadRequest, codeBadRequest)
	rec := env.do(http.MethodGet, "/v1/admin/device-groups?tenant_id="+tr.TenantID, padmin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform admin list = %d; body: %s", rec.Code, rec.Body.String())
	}
	// Another tenant sees nothing.
	other := env.createTenant(t, "other", "Other", nil)
	oTok := env.token(t, []string{"tenant_admin"}, other.TenantID)
	rec = env.do(http.MethodGet, "/v1/admin/device-groups", oTok, nil)
	var list groupListResponse
	env.decode(t, rec, &list)
	if list.Total != 0 {
		t.Fatalf("other tenant sees %d groups, want 0", list.Total)
	}
}

func TestDeviceGroupsNotWiredFailsClosed(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	// A server without the Groups seam answers 500 (fail closed).
	srv := NewServer(env.store.tenants, env.store.devices, env.store.keys, env.sessions)
	srv.Now = func() time.Time { return fixedNow }
	rec := doRaw(srv.Handler(), http.MethodPost, "/v1/admin/device-groups", tok, map[string]any{"name": "x"})
	env.assertError(t, rec, http.StatusInternalServerError, codeInternal)
}
