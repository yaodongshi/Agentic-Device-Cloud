package mcpbinding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// deviceMCP is a httptest-simulated class-A device MCP endpoint serving
// tools/list over the simplified JSON-RPC POST.
type deviceMCP struct {
	server   *httptest.Server
	mux      *http.ServeMux
	lastAuth string
	tools    []DeviceTool
	status   int
	rpcErr   *map[string]any
}

func newDeviceMCP(t *testing.T) *deviceMCP {
	t.Helper()
	d := &deviceMCP{
		status: http.StatusOK,
		tools: []DeviceTool{
			{Name: "get_spindle_status", Description: "Read spindle RPM", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "set_spindle_speed", Description: "Set spindle RPM", InputSchema: json.RawMessage(`{"type":"object"}`),
				RiskSuggestion: intPtr(2), SchemaVersion: "2"},
		},
		mux: http.NewServeMux(),
	}
	d.mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		d.lastAuth = r.Header.Get("Authorization")
		if d.status != http.StatusOK {
			http.Error(w, "boom", d.status)
			return
		}
		var req struct {
			JSONRPC string `json:"jsonrpc"`
			ID      string `json:"id"`
			Method  string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "tools/list" {
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if d.rpcErr != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "error": *d.rpcErr,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"tools": d.tools},
		})
	})
	d.server = httptest.NewServer(d.mux)
	t.Cleanup(d.server.Close)
	return d
}

func intPtr(v int) *int { return &v }

func TestSyncHTTPClientListTools(t *testing.T) {
	dev := newDeviceMCP(t)
	client := NewSyncHTTPClient(dev.server.Client())

	tools, err := client.ListTools(context.Background(), dev.server.URL+"/mcp", "at-token")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "get_spindle_status" || tools[1].Name != "set_spindle_speed" {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[1].SchemaVersion != "2" || tools[1].RiskSuggestion == nil || *tools[1].RiskSuggestion != 2 {
		t.Fatalf("extension fields lost: %+v", tools[1])
	}
	if dev.lastAuth != "Bearer at-token" {
		t.Fatalf("Authorization = %q, want Bearer token", dev.lastAuth)
	}
}

func TestSyncHTTPClientProtocolError(t *testing.T) {
	dev := newDeviceMCP(t)
	rpcErr := map[string]any{"code": -32601, "message": "method not found"}
	dev.rpcErr = &rpcErr
	client := NewSyncHTTPClient(dev.server.Client())

	_, err := client.ListTools(context.Background(), dev.server.URL+"/mcp", "at-token")
	if err == nil || !strings.Contains(err.Error(), "protocol error -32601") {
		t.Fatalf("err = %v, want protocol error", err)
	}
}

func TestSyncHTTPClientTransportError(t *testing.T) {
	dev := newDeviceMCP(t)
	dev.status = http.StatusUnauthorized
	client := NewSyncHTTPClient(dev.server.Client())

	_, err := client.ListTools(context.Background(), dev.server.URL+"/mcp", "bad-token")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401 error", err)
	}
}

func TestPGToolCatalogStoreReplaceTools(t *testing.T) {
	m := newBindingMockPool(t)
	store := NewPGToolCatalogStore(m)
	syncedAt := time.Date(2026, 8, 17, 8, 5, 0, 0, time.UTC)
	tools := []DeviceTool{
		{Name: "get_spindle_status", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "set_spindle_speed", Description: "write", InputSchema: json.RawMessage(`{"type":"object"}`),
			RiskSuggestion: intPtr(2), SchemaVersion: "2"},
	}
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta(sqlUpsertTool)).
		WithArgs(repTenantID, repDeviceID, "get_spindle_status", "read", `{"type":"object"}`, nil, `{}`, 2, "1.0", syncedAt).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectExec(regexp.QuoteMeta(sqlUpsertTool)).
		WithArgs(repTenantID, repDeviceID, "set_spindle_speed", "write", `{"type":"object"}`, nil, `{}`, 2, "2", syncedAt).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectExec(regexp.QuoteMeta(sqlDeleteStaleTools)).
		WithArgs(repDeviceID, []string{"get_spindle_status", "set_spindle_speed"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	m.ExpectCommit()

	n, err := store.ReplaceTools(context.Background(), repTenantID, repDeviceID, tools, syncedAt)
	if err != nil {
		t.Fatalf("ReplaceTools: %v", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGToolCatalogStoreEmptyCatalogDeletesAll(t *testing.T) {
	m := newBindingMockPool(t)
	store := NewPGToolCatalogStore(m)
	syncedAt := time.Date(2026, 8, 17, 8, 5, 0, 0, time.UTC)
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta(sqlDeleteStaleTools)).
		WithArgs(repDeviceID, []string{}).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))
	m.ExpectCommit()

	n, err := store.ReplaceTools(context.Background(), repTenantID, repDeviceID, nil, syncedAt)
	if err != nil {
		t.Fatalf("ReplaceTools: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// memCatalogStore is the in-memory ToolCatalogStore used by service
// tests; it records the upserted rows and mirrors the platform-authority
// preservation semantics for assertions.
type memCatalogStore struct {
	tools map[string][]DeviceTool // key: deviceID
}

func newMemCatalogStore() *memCatalogStore {
	return &memCatalogStore{tools: map[string][]DeviceTool{}}
}

func (s *memCatalogStore) ReplaceTools(_ context.Context, _, deviceID string, tools []DeviceTool, _ time.Time) (int, error) {
	cp := make([]DeviceTool, len(tools))
	copy(cp, tools)
	s.tools[deviceID] = cp
	return len(tools), nil
}

func TestToolSyncerEndToEnd(t *testing.T) {
	dev := newDeviceMCP(t)
	store := newMemCatalogStore()
	syncer := NewToolSyncer(NewSyncHTTPClient(dev.server.Client()), store)

	n, err := syncer.Sync(context.Background(), repDeviceID, repTenantID, dev.server.URL+"/mcp", "at-token")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	got := store.tools[repDeviceID]
	if len(got) != 2 || got[0].Name != "get_spindle_status" {
		t.Fatalf("stored tools = %+v", got)
	}
}
