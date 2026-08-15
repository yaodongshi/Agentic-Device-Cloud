//! Wire serialization tests: exact JSON output must match the Go reference
//! implementation (`core-sdk/protocol/protocol.go` json tags) byte for byte.

use adc_edge_sdk::wire::{
    Handshake, JSONRPCError, JSONRPCRequest, JSONRPCResponse, MCPTool, ToolCallParams,
    ToolCallResult, ToolContent, ToolsListResult, ERR_METHOD, HEADER_X_DEVICE_ID,
    HEADER_X_DEVICE_NONCE, HEADER_X_DEVICE_SIGNATURE, HEADER_X_DEVICE_TIMESTAMP, METHOD_TOOLS_CALL,
    METHOD_TOOLS_LIST, PROTOCOL_VERSION,
};
use serde_json::json;

#[test]
fn request_serializes_with_go_tags() {
    let request = JSONRPCRequest {
        jsonrpc: "2.0".into(),
        id: "g-0001".into(),
        method: METHOD_TOOLS_LIST.into(),
        params: None,
        version: Some(PROTOCOL_VERSION.into()),
    };
    let s = serde_json::to_string(&request).unwrap();
    assert_eq!(
        s,
        r#"{"jsonrpc":"2.0","id":"g-0001","method":"tools/list","version":"1.0"}"#
    );
}

#[test]
fn request_omits_absent_fields_like_go_omitempty() {
    let request = JSONRPCRequest {
        jsonrpc: "2.0".into(),
        id: "g-0002".into(),
        method: METHOD_TOOLS_CALL.into(),
        params: Some(json!({"name": "set_spindle_speed", "arguments": {"rpm": 4200}})),
        version: None,
    };
    let s = serde_json::to_string(&request).unwrap();
    assert!(!s.contains("version"));
    assert!(s.contains("\"params\""));
}

#[test]
fn response_result_and_error_variants() {
    let ok = JSONRPCResponse {
        jsonrpc: "2.0".into(),
        id: "g-0001".into(),
        result: Some(json!({"tools": []})),
        error: None,
    };
    assert_eq!(
        serde_json::to_string(&ok).unwrap(),
        r#"{"jsonrpc":"2.0","id":"g-0001","result":{"tools":[]}}"#
    );

    let err = JSONRPCResponse {
        jsonrpc: "2.0".into(),
        id: "g-0002".into(),
        result: None,
        error: Some(JSONRPCError {
            code: ERR_METHOD,
            message: "method not found: nope".into(),
            data: None,
        }),
    };
    assert_eq!(
        serde_json::to_string(&err).unwrap(),
        r#"{"jsonrpc":"2.0","id":"g-0002","error":{"code":-32601,"message":"method not found: nope"}}"#
    );
}

#[test]
fn mcp_tool_camel_case_fields_match_go_tags() {
    // Mirrors the mockdevice tools/list payload, plus riskLevel and
    // schemaVersion which only exist in the Go MCPTool struct.
    let tool = MCPTool {
        name: "set_spindle_speed".into(),
        description: Some("Set CNC spindle RPM".into()),
        input_schema: json!({"type": "object", "properties": {"rpm": {"type": "number"}}, "required": ["rpm"]}),
        risk_level: Some(2),
        schema_version: Some("1".into()),
    };
    let s = serde_json::to_string(&tool).unwrap();
    assert_eq!(
        s,
        r#"{"name":"set_spindle_speed","description":"Set CNC spindle RPM","inputSchema":{"type":"object","properties":{"rpm":{"type":"number"}},"required":["rpm"]},"riskLevel":2,"schemaVersion":"1"}"#
    );
    // The wire format must never leak snake_case names.
    assert!(!s.contains("input_schema"));
    assert!(!s.contains("risk_level"));
    assert!(!s.contains("schema_version"));
}

#[test]
fn mcp_tool_omits_optional_fields_when_absent() {
    let tool = MCPTool {
        name: "get_status".into(),
        description: None,
        input_schema: json!({"type": "object"}),
        risk_level: None,
        schema_version: None,
    };
    assert_eq!(
        serde_json::to_string(&tool).unwrap(),
        r#"{"name":"get_status","inputSchema":{"type":"object"}}"#
    );
}

#[test]
fn tools_list_result_roundtrips_design_33_example() {
    let result = ToolsListResult {
        tools: vec![MCPTool {
            name: "set_spindle_speed".into(),
            description: Some("Set CNC spindle RPM".into()),
            input_schema: json!({"type": "object", "properties": {"rpm": {"type": "number"}}, "required": ["rpm"]}),
            risk_level: Some(2),
            schema_version: Some("1".into()),
        }],
    };
    let s = serde_json::to_string(&result).unwrap();
    let back: ToolsListResult = serde_json::from_str(&s).unwrap();
    assert_eq!(back, result);
    assert_eq!(back.tools[0].name, "set_spindle_speed");
}

#[test]
fn tool_call_result_uses_is_error_tag() {
    let result = ToolCallResult {
        content: vec![ToolContent {
            content_type: "text".into(),
            text: Some("drive fault: servo overload".into()),
        }],
        is_error: true,
    };
    let s = serde_json::to_string(&result).unwrap();
    assert_eq!(
        s,
        r#"{"content":[{"type":"text","text":"drive fault: servo overload"}],"isError":true}"#
    );
    assert!(!s.contains("is_error"));
    // Round-trip back to the struct.
    let back: ToolCallResult = serde_json::from_str(&s).unwrap();
    assert!(back.is_error);
    assert_eq!(
        back.content[0].text.as_deref(),
        Some("drive fault: servo overload")
    );
}

#[test]
fn tool_call_params_parse_design_33_example() {
    // design/33 3.3: tools/call request params.
    let params: ToolCallParams =
        serde_json::from_str(r#"{"name":"set_spindle_speed","arguments":{"rpm":4200}}"#).unwrap();
    assert_eq!(params.name, "set_spindle_speed");
    assert_eq!(params.arguments["rpm"], json!(4200));
    // Missing arguments map parses as empty (Go nil map).
    let bare: ToolCallParams = serde_json::from_str(r#"{"name":"get_status"}"#).unwrap();
    assert!(bare.arguments.is_empty());
}

#[test]
fn handshake_version_negotiation_fields() {
    let handshake = Handshake {
        protocol_version: "1.0".into(),
        capabilities: Some(vec!["tools".into(), "heartbeat".into()]),
    };
    let s = serde_json::to_string(&handshake).unwrap();
    assert_eq!(
        s,
        r#"{"protocolVersion":"1.0","capabilities":["tools","heartbeat"]}"#
    );
}

#[test]
fn header_and_method_constants_match_go_protocol() {
    // Hardcoded against core-sdk/protocol/protocol.go.
    assert_eq!(HEADER_X_DEVICE_ID, "X-Device-ID");
    assert_eq!(HEADER_X_DEVICE_TIMESTAMP, "X-Device-Timestamp");
    assert_eq!(HEADER_X_DEVICE_NONCE, "X-Device-Nonce");
    assert_eq!(HEADER_X_DEVICE_SIGNATURE, "X-Device-Signature");
    assert_eq!(METHOD_TOOLS_LIST, "tools/list");
    assert_eq!(METHOD_TOOLS_CALL, "tools/call");
    assert_eq!(PROTOCOL_VERSION, "1.0");
}

#[test]
fn notification_without_id_parses() {
    // kick/heartbeat_ack notifications have no id (Go struct: zero value).
    let notification: JSONRPCRequest = serde_json::from_str(
        r#"{"jsonrpc":"2.0","method":"kick","params":{"reason":"replaced_by_new_connection"}}"#,
    )
    .unwrap();
    assert!(notification.id.is_empty());
    assert_eq!(notification.method, "kick");
    assert_eq!(
        notification.params.unwrap()["reason"],
        "replaced_by_new_connection"
    );
}
