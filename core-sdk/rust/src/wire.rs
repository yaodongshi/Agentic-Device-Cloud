//! ADC device-side wire protocol types (JSON-RPC 2.0 style, MCP semantics).
//!
//! This module is the Rust mirror of `core-sdk/protocol/protocol.go`. Serde
//! field names must match the Go `json` tags exactly: `inputSchema`,
//! `riskLevel`, `schemaVersion` and `isError` stay camelCase on the wire even
//! though the Rust struct fields are snake_case.

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// The wire protocol version this SDK speaks. Requests or handshakes without
/// a `version`/`protocolVersion` field are treated as 1.0 for backward
/// compatibility (GAP-06).
pub const PROTOCOL_VERSION: &str = "1.0";

/// Standard JSON-RPC 2.0 error codes.
pub const ERR_PARSE: i32 = -32700;
pub const ERR_INVALID_REQ: i32 = -32600;
pub const ERR_METHOD: i32 = -32601;
pub const ERR_INVALID_ARG: i32 = -32602;
pub const ERR_INTERNAL: i32 = -32603;

/// Implementation-defined error: peer advertises an unsupported protocol
/// version (GAP-06).
pub const ERR_VERSION_UNSUPPORTED: i32 = -32001;

/// In-tunnel JSON-RPC method names (core-sdk/protocol + design/33 3.3).
pub const METHOD_TOOLS_LIST: &str = "tools/list";
pub const METHOD_TOOLS_CALL: &str = "tools/call";
pub const METHOD_HEARTBEAT: &str = "heartbeat";
pub const METHOD_HEARTBEAT_ACK: &str = "heartbeat_ack";
pub const METHOD_KICK: &str = "kick";

/// Device-facing auth handshake headers (SEC-03, core-sdk/protocol).
pub const HEADER_X_DEVICE_ID: &str = "X-Device-ID";
pub const HEADER_X_DEVICE_TIMESTAMP: &str = "X-Device-Timestamp";
pub const HEADER_X_DEVICE_NONCE: &str = "X-Device-Nonce";
pub const HEADER_X_DEVICE_SIGNATURE: &str = "X-Device-Signature";

/// Auth timestamp drift window in seconds (SEC-03).
pub const AUTH_TIME_WINDOW_SEC: i64 = 300;

/// JSON-RPC 2.0 request. `params`/`version` are omitted when absent, exactly
/// like the Go `omitempty` tags.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct JSONRPCRequest {
    pub jsonrpc: String,
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub method: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub params: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub version: Option<String>,
}

impl JSONRPCRequest {
    /// Returns the effective wire protocol version, defaulting to
    /// [`PROTOCOL_VERSION`] when the field is absent (backward compatibility).
    pub fn effective_version(&self) -> &str {
        self.version.as_deref().unwrap_or(PROTOCOL_VERSION)
    }
}

/// JSON-RPC 2.0 response.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct JSONRPCResponse {
    pub jsonrpc: String,
    pub id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<JSONRPCError>,
}

/// JSON-RPC 2.0 error object.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct JSONRPCError {
    pub code: i32,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data: Option<Value>,
}

/// Connection-opening handshake a device sends after the transport is
/// established (GAP-06). The server accepts it or rejects it with a JSON-RPC
/// error whose code is [`ERR_VERSION_UNSUPPORTED`].
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Handshake {
    #[serde(rename = "protocolVersion")]
    pub protocol_version: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub capabilities: Option<Vec<String>>,
}

/// Validates the version advertised by a peer. An empty version is treated as
/// 1.0 (backward compatibility); any other value is rejected with
/// [`ERR_VERSION_UNSUPPORTED`].
pub fn negotiate_version(version: &str) -> Result<(), JSONRPCError> {
    if version.is_empty() || version == PROTOCOL_VERSION {
        return Ok(());
    }
    Err(JSONRPCError {
        code: ERR_VERSION_UNSUPPORTED,
        message: format!("unsupported protocol version: {version}"),
        data: None,
    })
}

/// MCP tool definition. `riskLevel` is the device-suggested risk level (0-3);
/// the platform DB is authoritative (SEC-09). `schemaVersion` feeds the
/// compatibility matrix (SEC-22).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct MCPTool {
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(rename = "inputSchema")]
    pub input_schema: Value,
    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(rename = "riskLevel")]
    pub risk_level: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    #[serde(rename = "schemaVersion")]
    pub schema_version: Option<String>,
}

impl MCPTool {
    /// Effective risk level: defaults to 2 (approval required before use)
    /// when the device did not report one.
    pub fn effective_risk(&self) -> i32 {
        self.risk_level.unwrap_or(2)
    }
}

/// Result payload of a `tools/list` response.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolsListResult {
    pub tools: Vec<MCPTool>,
}

/// Params payload of a `tools/call` request.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Default)]
pub struct ToolCallParams {
    pub name: String,
    #[serde(default)]
    pub arguments: serde_json::Map<String, Value>,
}

/// Result payload of a `tools/call` response. `isError=true` is a business
/// failure, not a protocol error.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolCallResult {
    pub content: Vec<ToolContent>,
    #[serde(rename = "isError")]
    pub is_error: bool,
}

/// A single content block of a tool call result.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolContent {
    #[serde(rename = "type")]
    pub content_type: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub text: Option<String>,
}

impl ToolContent {
    /// Convenience constructor for `{"type":"text","text":...}` blocks.
    pub fn text(text: impl Into<String>) -> Self {
        Self {
            content_type: "text".to_string(),
            text: Some(text.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn negotiate_version_accepts_current_and_empty() {
        assert!(negotiate_version("1.0").is_ok());
        assert!(negotiate_version("").is_ok());
    }

    #[test]
    fn negotiate_version_rejects_unknown() {
        let err = negotiate_version("2.0").unwrap_err();
        assert_eq!(err.code, ERR_VERSION_UNSUPPORTED);
        assert!(err.message.contains("2.0"));
    }

    #[test]
    fn default_risk_is_two() {
        let tool = MCPTool {
            name: "t".into(),
            description: None,
            input_schema: serde_json::json!({}),
            risk_level: None,
            schema_version: None,
        };
        assert_eq!(tool.effective_risk(), 2);
    }

    #[test]
    fn effective_version_defaults() {
        let req = JSONRPCRequest {
            jsonrpc: "2.0".into(),
            id: "1".into(),
            method: "tools/list".into(),
            params: None,
            version: None,
        };
        assert_eq!(req.effective_version(), PROTOCOL_VERSION);
    }
}
