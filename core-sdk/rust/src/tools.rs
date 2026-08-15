//! Local tool registry: tool definitions plus dispatch-by-name for incoming
//! `tools/call` requests.
//!
//! A [`ToolRegistry`] owns the tool metadata reported to the platform in
//! response to `tools/list`, and dispatches `tools/call` arguments to
//! registered handlers. Handlers are plain closures returning a
//! [`ToolCallResult`]; a panic inside a handler is caught and converted into
//! an `isError` result so a misbehaving tool never kills the connection loop.

use std::collections::HashMap;
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::{Arc, RwLock};

use serde_json::Value;

use crate::wire::{MCPTool, ToolCallResult, ToolContent};

/// Signature of a local tool handler: receives the `arguments` map of the
/// `tools/call` params and returns the result payload.
pub type ToolHandler = Arc<dyn Fn(&serde_json::Map<String, Value>) -> ToolCallResult + Send + Sync>;

/// Metadata of a local tool, mirrored into the `tools/list` response.
#[derive(Debug, Clone)]
pub struct ToolDef {
    pub name: String,
    pub description: Option<String>,
    pub input_schema: Value,
    pub risk_level: Option<i32>,
    pub schema_version: Option<String>,
}

impl ToolDef {
    /// Creates a tool definition with a default `{"type":"object"}` input
    /// schema and no risk level (the platform then defaults to 2).
    pub fn new(name: impl Into<String>, description: impl Into<String>) -> Self {
        Self {
            name: name.into(),
            description: Some(description.into()),
            input_schema: serde_json::json!({"type": "object"}),
            risk_level: None,
            schema_version: None,
        }
    }

    /// Sets the device-suggested risk level (0-3, SEC-09).
    pub fn with_risk(mut self, level: i32) -> Self {
        self.risk_level = Some(level);
        self
    }

    /// Overrides the input JSON schema.
    pub fn with_schema(mut self, schema: Value) -> Self {
        self.input_schema = schema;
        self
    }

    /// Sets the tool schema version for the compatibility matrix (SEC-22).
    pub fn with_schema_version(mut self, version: impl Into<String>) -> Self {
        self.schema_version = Some(version.into());
        self
    }

    /// Converts to the wire type reported in `tools/list`.
    pub fn to_mcp_tool(&self) -> MCPTool {
        MCPTool {
            name: self.name.clone(),
            description: self.description.clone(),
            input_schema: self.input_schema.clone(),
            risk_level: self.risk_level,
            schema_version: self.schema_version.clone(),
        }
    }
}

/// Error returned by [`ToolRegistry::register`].
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum ToolRegistryError {
    #[error("tool already registered: {0}")]
    Duplicate(String),
}

/// Thread-safe registry of local tools.
#[derive(Clone, Default)]
pub struct ToolRegistry {
    tools: Arc<RwLock<HashMap<String, (ToolDef, ToolHandler)>>>,
}

impl ToolRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Registers a tool definition and its handler. Returns an error when a
    /// tool with the same name is already registered.
    pub fn register(
        &self,
        def: ToolDef,
        handler: impl Fn(&serde_json::Map<String, Value>) -> ToolCallResult + Send + Sync + 'static,
    ) -> Result<(), ToolRegistryError> {
        let mut tools = self.tools.write().expect("tool registry lock poisoned");
        if tools.contains_key(&def.name) {
            return Err(ToolRegistryError::Duplicate(def.name.clone()));
        }
        tools.insert(def.name.clone(), (def, Arc::new(handler)));
        Ok(())
    }

    /// Returns the wire tool list for a `tools/list` response, sorted by name
    /// for deterministic output.
    pub fn tool_defs(&self) -> Vec<MCPTool> {
        let tools = self.tools.read().expect("tool registry lock poisoned");
        let mut defs: Vec<MCPTool> = tools.values().map(|(def, _)| def.to_mcp_tool()).collect();
        defs.sort_by(|a, b| a.name.cmp(&b.name));
        defs
    }

    /// Whether a tool with the given name is registered.
    pub fn contains(&self, name: &str) -> bool {
        self.tools
            .read()
            .expect("tool registry lock poisoned")
            .contains_key(name)
    }

    /// Dispatches a `tools/call` to the named handler.
    ///
    /// Unknown tools yield `isError=true` with a descriptive text block.
    /// A panicking handler is caught and reported as `isError=true`.
    pub fn call(&self, name: &str, args: &serde_json::Map<String, Value>) -> ToolCallResult {
        let handler = {
            let tools = self.tools.read().expect("tool registry lock poisoned");
            tools.get(name).map(|(_, handler)| handler.clone())
        };
        match handler {
            Some(handler) => match catch_unwind(AssertUnwindSafe(|| handler(args))) {
                Ok(result) => result,
                Err(_) => ToolCallResult {
                    content: vec![ToolContent::text(format!("tool panicked: {name}"))],
                    is_error: true,
                },
            },
            None => ToolCallResult {
                content: vec![ToolContent::text(format!("unknown tool: {name}"))],
                is_error: true,
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ok_result(text: &str) -> ToolCallResult {
        ToolCallResult {
            content: vec![ToolContent::text(text)],
            is_error: false,
        }
    }

    #[test]
    fn register_and_list_tools() {
        let registry = ToolRegistry::new();
        registry
            .register(ToolDef::new("get_status", "read-only").with_risk(0), |_| {
                ok_result("ok")
            })
            .unwrap();
        registry
            .register(
                ToolDef::new("set_speed", "high risk")
                    .with_risk(2)
                    .with_schema(serde_json::json!({"type": "object"})),
                |_| ok_result("ok"),
            )
            .unwrap();

        let defs = registry.tool_defs();
        assert_eq!(defs.len(), 2);
        assert_eq!(defs[0].name, "get_status");
        assert_eq!(defs[1].name, "set_speed");
        assert_eq!(defs[1].risk_level, Some(2));
        assert_eq!(defs[1].effective_risk(), 2);
        assert!(registry.contains("get_status"));
    }

    #[test]
    fn duplicate_register_fails() {
        let registry = ToolRegistry::new();
        registry
            .register(ToolDef::new("t", "d"), |_| ok_result("ok"))
            .unwrap();
        let err = registry
            .register(ToolDef::new("t", "d"), |_| ok_result("ok"))
            .unwrap_err();
        assert_eq!(err, ToolRegistryError::Duplicate("t".into()));
    }

    #[test]
    fn dispatch_passes_arguments_through() {
        let registry = ToolRegistry::new();
        registry
            .register(ToolDef::new("echo", "d"), |args| {
                let value = args.get("v").cloned().unwrap_or(Value::Null);
                ok_result(&value.to_string())
            })
            .unwrap();

        let mut args = serde_json::Map::new();
        args.insert("v".into(), serde_json::json!(42));
        let result = registry.call("echo", &args);
        assert!(!result.is_error);
        assert_eq!(result.content[0].text.as_deref(), Some("42"));
    }

    #[test]
    fn dispatch_unknown_tool_is_error() {
        let registry = ToolRegistry::new();
        let result = registry.call("missing", &serde_json::Map::new());
        assert!(result.is_error);
        assert!(result.content[0]
            .text
            .as_deref()
            .unwrap()
            .contains("unknown tool"));
    }

    #[test]
    fn dispatch_catches_handler_panic() {
        let registry = ToolRegistry::new();
        registry
            .register(ToolDef::new("boom", "d"), |_| panic!("kaboom"))
            .unwrap();
        let result = registry.call("boom", &serde_json::Map::new());
        assert!(result.is_error);
        assert!(result.content[0]
            .text
            .as_deref()
            .unwrap()
            .contains("panicked"));
    }
}
