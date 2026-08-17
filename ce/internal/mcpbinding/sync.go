// Post-binding tool catalog synchronization (doc/07 step 5, design/82
// B5.3). The platform calls tools/list on the device MCP endpoint using
// the simplified JSON-RPC POST form compatible with the core-sdk
// protocol (protocol.MethodToolsList), carrying the OAuth Bearer token.
// The reported catalog is written to adc_device_tools in one transaction:
// reported tools are upserted (device-declared definitions replace the
// stored ones while the platform-authoritative risk_level, is_enabled and
// display_name survive, SEC-09) and tools the device no longer reports
// are deleted (doc/07 2.4: the device catalog is authoritative for the
// definition, the platform for governance metadata).

package mcpbinding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"adc.dev/core-sdk/protocol"
)

// mcpProtocolVersion is the MCP protocol version advertised on the
// simplified JSON-RPC POST (doc/07: 2025-06-18 baseline, S5).
const mcpProtocolVersion = "2025-06-18"

// DeviceTool is one tool definition from a device tools/list response
// (MCP 2025-06-18 tool shape plus the x-adc extension fields the PoC
// protocol already carries). RiskSuggestion is the device-declared
// initial risk level only; the platform risk_level in adc_device_tools
// is authoritative (doc/07 2.4, SEC-09).
type DeviceTool struct {
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	InputSchema    json.RawMessage `json:"inputSchema"`
	OutputSchema   json.RawMessage `json:"outputSchema,omitempty"`
	Annotations    json.RawMessage `json:"annotations,omitempty"`
	RiskSuggestion *int            `json:"riskLevel,omitempty"`
	SchemaVersion  string          `json:"schemaVersion,omitempty"`
}

// toolsListResult mirrors the tools/list result envelope.
type toolsListResult struct {
	Tools []DeviceTool `json:"tools"`
}

// SyncHTTPClient calls tools/list over the device MCP endpoint.
type SyncHTTPClient struct {
	HTTP HTTPClient
	Now  func() time.Time
}

// NewSyncHTTPClient builds a sync client; a nil client falls back to
// http.DefaultClient.
func NewSyncHTTPClient(hc HTTPClient) *SyncHTTPClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &SyncHTTPClient{HTTP: hc}
}

func (c *SyncHTTPClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// ListTools posts the tools/list JSON-RPC request to the device MCP
// endpoint with the Bearer token (doc/07 step 5). Protocol errors (MCP
// error object) and transport errors are both reported as errors; the
// device answering isError is not applicable to tools/list.
func (c *SyncHTTPClient) ListTools(ctx context.Context, mcpEndpoint, bearerToken string) ([]DeviceTool, error) {
	reqID := fmt.Sprintf("sync-%d", c.now().UnixNano())
	payload, err := json.Marshal(protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  protocol.MethodToolsList,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcpbinding: tools/list transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcpbinding: tools/list answered %d", resp.StatusCode)
	}
	var env protocol.JSONRPCResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaBytes)).Decode(&env); err != nil {
		return nil, fmt.Errorf("mcpbinding: decode tools/list response: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("mcpbinding: tools/list protocol error %d: %s", env.Error.Code, env.Error.Message)
	}
	var result toolsListResult
	if err := json.Unmarshal(env.Result, &result); err != nil {
		return nil, fmt.Errorf("mcpbinding: decode tools/list result: %w", err)
	}
	return result.Tools, nil
}

// ToolCatalogStore persists one device's authoritative tool catalog.
type ToolCatalogStore interface {
	// ReplaceTools upserts the reported tools and deletes the
	// unreported ones for the device in one transaction.
	ReplaceTools(ctx context.Context, tenantID, deviceID string, tools []DeviceTool, syncedAt time.Time) (int, error)
}

// pgToolCatalogStore is the PostgreSQL ToolCatalogStore (design/32 3.6).
type pgToolCatalogStore struct {
	pool pooler
}

// NewPGToolCatalogStore builds a catalog store over an existing pool.
func NewPGToolCatalogStore(pool pooler) *pgToolCatalogStore {
	return &pgToolCatalogStore{pool: pool}
}

const (
	sqlUpsertTool = `INSERT INTO adc_device_tools
		(tenant_id, device_id, tool_name, display_name, description, input_schema,
		 output_schema, annotations, risk_level, schema_version, is_enabled, last_seen_at)
		VALUES ($1::uuid, $2::uuid, $3, $3, $4, $5, $6, $7, $8, $9, TRUE, $10)
		ON CONFLICT (device_id, tool_name) DO UPDATE SET
			description = EXCLUDED.description,
			input_schema = EXCLUDED.input_schema,
			output_schema = EXCLUDED.output_schema,
			annotations = EXCLUDED.annotations,
			schema_version = EXCLUDED.schema_version,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = now()`

	sqlDeleteStaleTools = `DELETE FROM adc_device_tools
		WHERE device_id = $1::uuid AND tool_name <> ALL($2::text[])`
)

// defaultRiskLevel is the fail-safe risk for tools that declare no
// suggestion: 2 = high, approve-before-use (design/32 3.6 default).
const defaultRiskLevel = 2

// ReplaceTools implements ToolCatalogStore. Device-declared definition
// fields (description/schema/annotations/version) are refreshed, while
// the platform governance fields (risk_level, is_enabled, display_name)
// are intentionally absent from the DO UPDATE set so a re-sync never
// clobbers an admin decision (SEC-09 authority boundary).
func (s *pgToolCatalogStore) ReplaceTools(ctx context.Context, tenantID, deviceID string, tools []DeviceTool, syncedAt time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	names := make([]string, 0, len(tools))
	for _, t := range tools {
		input := t.InputSchema
		if len(input) == 0 {
			input = json.RawMessage(`{"type":"object"}`)
		}
		var output any
		if len(t.OutputSchema) > 0 {
			output = string(t.OutputSchema)
		}
		annotations := t.Annotations
		if len(annotations) == 0 {
			annotations = json.RawMessage(`{}`)
		}
		risk := defaultRiskLevel
		if t.RiskSuggestion != nil && *t.RiskSuggestion >= 0 && *t.RiskSuggestion <= 3 {
			risk = *t.RiskSuggestion
		}
		version := t.SchemaVersion
		if version == "" {
			version = "1.0"
		}
		if _, err := tx.Exec(ctx, sqlUpsertTool,
			tenantID, deviceID, t.Name, t.Description, string(input), output, string(annotations), risk, version, syncedAt); err != nil {
			return 0, err
		}
		names = append(names, t.Name)
	}
	if _, err := tx.Exec(ctx, sqlDeleteStaleTools, deviceID, names); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(tools), nil
}

// ToolSyncer is the seam the service consumes: fetch and persist one
// device's tool catalog (design/82 B5.3).
type ToolSyncer interface {
	Sync(ctx context.Context, deviceID, tenantID, mcpEndpoint, bearerToken string) (int, error)
}

// toolSyncer wires the HTTP client to the catalog store.
type toolSyncer struct {
	client *SyncHTTPClient
	store  ToolCatalogStore
}

// NewToolSyncer builds the default syncer.
func NewToolSyncer(client *SyncHTTPClient, store ToolCatalogStore) ToolSyncer {
	return &toolSyncer{client: client, store: store}
}

// Sync fetches tools/list with the Bearer token and persists the
// catalog; the reported tool count is returned.
func (t *toolSyncer) Sync(ctx context.Context, deviceID, tenantID, mcpEndpoint, bearerToken string) (int, error) {
	tools, err := t.client.ListTools(ctx, mcpEndpoint, bearerToken)
	if err != nil {
		return 0, fmt.Errorf("mcpbinding: sync tools: %w", err)
	}
	n, err := t.store.ReplaceTools(ctx, tenantID, deviceID, tools, t.client.now().UTC())
	if err != nil {
		return 0, fmt.Errorf("mcpbinding: persist tools: %w", err)
	}
	return n, nil
}
