//! adc-edge-sdk: ADC edge SDK reference implementation for B-class devices
//! (Legacy Bridge).
//!
//! The SDK connects to the platform WSS reverse tunnel (`/v1/devices/tunnel`,
//! design/33 3.3), authenticates with HMAC-SHA256 headers (SEC-03), answers
//! `tools/list` from a local [`tools::ToolRegistry`], dispatches `tools/call`
//! to registered handlers and reconnects with exponential backoff
//! (1/2/4/8/16/30/60 seconds + jitter, design/31 3.1.8).
//!
//! Wire types in [`wire`] mirror the JSON tags of the Go reference
//! implementation in `core-sdk/protocol` exactly.

pub mod auth;
pub mod client;
pub mod tools;
pub mod wire;
