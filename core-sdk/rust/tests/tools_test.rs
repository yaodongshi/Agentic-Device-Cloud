//! Tool dispatch integration tests: registry registration, tools/list
//! projection and tools/call dispatch with wire-format assertions.

use adc_edge_sdk::tools::{ToolDef, ToolRegistry, ToolRegistryError};
use adc_edge_sdk::wire::{ToolCallResult, ToolContent};
use serde_json::json;

fn ok(text: &str) -> ToolCallResult {
    ToolCallResult {
        content: vec![ToolContent::text(text)],
        is_error: false,
    }
}

#[test]
fn dispatch_read_only_tool() {
    let registry = ToolRegistry::new();
    registry
        .register(
            ToolDef::new("get_status", "Read device status").with_risk(0),
            |_| ok("online"),
        )
        .unwrap();
    let result = registry.call("get_status", &serde_json::Map::new());
    assert!(!result.is_error);
    assert_eq!(result.content[0].content_type, "text");
    assert_eq!(result.content[0].text.as_deref(), Some("online"));
}

#[test]
fn dispatch_high_risk_tool_receives_arguments() {
    let registry = ToolRegistry::new();
    registry
        .register(
            ToolDef::new("set_speed", "Set spindle RPM")
                .with_risk(2)
                .with_schema(json!({"type": "object", "properties": {"rpm": {"type": "number"}}, "required": ["rpm"]})),
            |args| {
                let rpm = args.get("rpm").and_then(|v| v.as_f64()).unwrap_or(-1.0);
                ok(&format!("set to {rpm}"))
            },
        )
        .unwrap();

    let mut args = serde_json::Map::new();
    args.insert("rpm".into(), json!(4200));
    let result = registry.call("set_speed", &args);
    assert!(!result.is_error);
    assert_eq!(result.content[0].text.as_deref(), Some("set to 4200"));
}

#[test]
fn tool_list_projection_uses_wire_field_names() {
    let registry = ToolRegistry::new();
    registry
        .register(
            ToolDef::new("get_status", "Read device status")
                .with_risk(0)
                .with_schema_version("1"),
            |_| ok("online"),
        )
        .unwrap();

    let defs = registry.tool_defs();
    assert_eq!(defs.len(), 1);
    let serialized = serde_json::to_string(&defs[0]).unwrap();
    assert!(serialized.contains("\"riskLevel\":0"), "{serialized}");
    assert!(
        serialized.contains("\"schemaVersion\":\"1\""),
        "{serialized}"
    );
    assert!(serialized.contains("\"inputSchema\""), "{serialized}");
}

#[test]
fn unknown_tool_dispatch_reports_error_result() {
    let registry = ToolRegistry::new();
    let result = registry.call("nope", &serde_json::Map::new());
    assert!(result.is_error);
    assert!(result.content[0]
        .text
        .as_deref()
        .unwrap()
        .contains("unknown tool: nope"));
}

#[test]
fn duplicate_registration_is_rejected() {
    let registry = ToolRegistry::new();
    registry
        .register(ToolDef::new("t", "d"), |_| ok("ok"))
        .unwrap();
    match registry.register(ToolDef::new("t", "d"), |_| ok("ok")) {
        Err(ToolRegistryError::Duplicate(name)) => assert_eq!(name, "t"),
        other => panic!("expected duplicate error, got {other:?}"),
    }
}

#[test]
fn registry_is_shareable_across_threads() {
    let registry = ToolRegistry::new();
    registry
        .register(ToolDef::new("t", "d"), |_| ok("ok"))
        .unwrap();
    let cloned = registry.clone();
    let handle = std::thread::spawn(move || cloned.call("t", &serde_json::Map::new()));
    assert!(!handle.join().unwrap().is_error);
}
