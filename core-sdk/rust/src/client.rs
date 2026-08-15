//! `DeviceClient`: the WSS reverse-tunnel client for B-class devices.
//!
//! Connection lifecycle (design/33 3.3, design/31 3.1.8):
//!
//! 1. Dial the tunnel URL with the HMAC auth headers (SEC-03, fresh nonce and
//!    timestamp per attempt).
//! 2. Answer the platform's `tools/list` from the [`ToolRegistry`] (the
//!    platform issues it within 5 seconds of connect).
//! 3. Dispatch `tools/call` requests to registered handlers.
//! 4. Send an application-level `heartbeat` notification every
//!    [`ClientConfig::heartbeat_interval`]; the platform acks with
//!    `heartbeat_ack`. WebSocket pings from the server are answered
//!    automatically by the underlying tungstenite stack.
//! 5. On disconnect, reconnect with exponential backoff over the sequence
//!    1/2/4/8/16/30/60 seconds with +/- jitter. A `retry_after_secs` hint in
//!    the close frame overrides the ladder for that attempt (design/31
//!    3.1.8). Every attempt performs a fresh WSS handshake (new headers) and
//!    re-registers tools by answering `tools/list` again.
//!
//! A single tokio task owns the socket: it selects on inbound frames, the
//! outbound mpsc queue and the heartbeat ticker, so frames never interleave
//! and tungstenite's automatic pong responses flush immediately.

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use futures_util::{SinkExt, StreamExt};
use http::header::HeaderValue;
use rand::Rng;
use thiserror::Error;
use tokio::sync::mpsc;
use tokio_tungstenite::connect_async;
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::Message;

use crate::auth::{generate_nonce, hmac_sign};
use crate::tools::ToolRegistry;
use crate::wire::{
    JSONRPCError, JSONRPCRequest, JSONRPCResponse, ToolCallParams, ToolsListResult, ERR_METHOD,
    HEADER_X_DEVICE_ID, HEADER_X_DEVICE_NONCE, HEADER_X_DEVICE_SIGNATURE,
    HEADER_X_DEVICE_TIMESTAMP, METHOD_HEARTBEAT, METHOD_HEARTBEAT_ACK, METHOD_KICK,
    METHOD_TOOLS_CALL, METHOD_TOOLS_LIST,
};

/// Default application heartbeat interval (the platform renews routing TTL
/// every 30 seconds, design/31 3.1.7).
pub const DEFAULT_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(30);
/// Default connect timeout.
pub const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
/// Default reconnect jitter amplitude (+/- fraction of the base delay).
pub const DEFAULT_JITTER: f64 = 0.2;
/// Reconnect backoff ladder in seconds (design/31 3.1.8): 1/2/4/8/16/30/60,
/// 60 repeats for any further attempt.
pub const BACKOFF_SEQUENCE_SECS: [u64; 7] = [1, 2, 4, 8, 16, 30, 60];

/// Returns the base reconnect delay (no jitter) for a reconnect `attempt`,
/// where attempt 0 is the first retry.
pub fn backoff_delay(attempt: u32) -> Duration {
    let idx = attempt as usize;
    let secs = if idx < BACKOFF_SEQUENCE_SECS.len() {
        BACKOFF_SEQUENCE_SECS[idx]
    } else {
        BACKOFF_SEQUENCE_SECS[BACKOFF_SEQUENCE_SECS.len() - 1]
    };
    Duration::from_secs(secs)
}

/// Applies a multiplicative jitter `factor` in `[-1.0, 1.0]` to a base delay
/// (the client samples it uniformly from +/- [`ClientConfig::jitter`]).
pub fn jittered(base: Duration, factor: f64) -> Duration {
    let secs = base.as_secs_f64() * (1.0 + factor);
    Duration::from_secs_f64(secs.max(0.0))
}

/// Client configuration. All fields have sensible defaults; only
/// `tunnel_url`, `device_id` and `secret` are required.
#[derive(Debug, Clone)]
pub struct ClientConfig {
    /// WSS tunnel endpoint, e.g. `ws://127.0.0.1:18080/v1/devices/tunnel`
    /// (dev without TLS) or `wss://...` (production).
    pub tunnel_url: String,
    /// Device code from the platform device registry.
    pub device_id: String,
    /// Device secret (plain, injected locally; never logged).
    pub secret: String,
    /// Application-level heartbeat period.
    pub heartbeat_interval: Duration,
    /// Connect timeout per attempt.
    pub connect_timeout: Duration,
    /// Reconnect jitter amplitude (e.g. 0.2 = +/- 20%).
    pub jitter: f64,
}

impl ClientConfig {
    pub fn new(
        tunnel_url: impl Into<String>,
        device_id: impl Into<String>,
        secret: impl Into<String>,
    ) -> Self {
        Self {
            tunnel_url: tunnel_url.into(),
            device_id: device_id.into(),
            secret: secret.into(),
            heartbeat_interval: DEFAULT_HEARTBEAT_INTERVAL,
            connect_timeout: DEFAULT_CONNECT_TIMEOUT,
            jitter: DEFAULT_JITTER,
        }
    }
}

/// Lifecycle events reported to the optional event handler.
#[derive(Debug, Clone)]
pub enum Event {
    /// A connect attempt is starting (`attempt` counts consecutive failed
    /// attempts).
    Connecting { attempt: u32, url: String },
    /// The tunnel is established and authenticated.
    Connected,
    /// A `tools/list` request was answered (`tool_count` tools reported).
    ToolsListServed {
        requested_id: String,
        tool_count: usize,
    },
    /// A `tools/call` request was dispatched and answered.
    ToolCallServed {
        requested_id: String,
        tool: String,
        is_error: bool,
    },
    /// A `heartbeat` notification was sent.
    HeartbeatSent,
    /// The platform kicked this connection (replaced by a newer one or
    /// credential revoked, design/33 3.3).
    Kicked { reason: Option<String> },
    /// The tunnel ended; a reconnect is scheduled after `retry_in`.
    Disconnected {
        reason: String,
        retry_in: Duration,
        attempt: u32,
    },
    /// An in-tunnel notification arrived (e.g. `heartbeat_ack`).
    Notification { method: String },
}

/// Fatal client error. Transient connect/disconnect failures are handled by
/// the internal reconnect loop and never surface here.
#[derive(Debug, Error)]
pub enum ClientError {
    #[error("invalid tunnel url: {0}")]
    InvalidUrl(String),
    #[error("connect failed: {0}")]
    Connect(String),
    #[error("websocket error: {0}")]
    Ws(#[from] tokio_tungstenite::tungstenite::Error),
    #[error("serialization error: {0}")]
    Json(#[from] serde_json::Error),
}

/// How a served connection ended (internal).
enum ServeEnd {
    Kicked { reason: Option<String> },
    Closed { hint: Option<u64>, reason: String },
    Failed { reason: String },
}

impl ServeEnd {
    fn reason(&self) -> String {
        match self {
            ServeEnd::Kicked { reason } => format!(
                "kicked by server ({})",
                reason.as_deref().unwrap_or("unknown reason")
            ),
            ServeEnd::Closed { reason, .. } => format!("closed by server ({reason})"),
            ServeEnd::Failed { reason } => reason.clone(),
        }
    }

    fn retry_hint(&self) -> Option<u64> {
        match self {
            ServeEnd::Closed { hint, .. } => *hint,
            _ => None,
        }
    }
}

type EventHandler = Arc<dyn Fn(&Event) + Send + Sync>;

/// The device tunnel client. Create with [`DeviceClient::new`], optionally
/// attach an event handler with [`DeviceClient::with_event_handler`] and
/// await [`DeviceClient::run`].
pub struct DeviceClient {
    config: ClientConfig,
    registry: Arc<ToolRegistry>,
    on_event: Option<EventHandler>,
}

impl DeviceClient {
    pub fn new(config: ClientConfig, registry: Arc<ToolRegistry>) -> Self {
        Self {
            config,
            registry,
            on_event: None,
        }
    }

    /// Attaches an event callback (connection lifecycle, tool sync and tool
    /// call logging). Called from the connection task; keep it fast.
    pub fn with_event_handler(mut self, handler: impl Fn(&Event) + Send + Sync + 'static) -> Self {
        self.on_event = Some(Arc::new(handler));
        self
    }

    /// Runs the connect/serve/reconnect loop forever. Returns only on fatal
    /// errors (bad URL/headers); every disconnect is retried with backoff.
    pub async fn run(&self) -> Result<(), ClientError> {
        let mut attempt: u32 = 0;
        loop {
            self.emit(&Event::Connecting {
                attempt,
                url: self.config.tunnel_url.clone(),
            });
            match self.connect_and_serve().await {
                Ok(end) => {
                    // The tunnel was established and then ended: restart the
                    // backoff ladder. A server retry hint (design/31 3.1.8)
                    // wins over the ladder for this attempt.
                    let delay = match end.retry_hint() {
                        Some(secs) => Duration::from_secs(secs),
                        None => self.jittered(backoff_delay(0)),
                    };
                    attempt = 1;
                    self.emit(&Event::Disconnected {
                        reason: end.reason(),
                        retry_in: delay,
                        attempt: 0,
                    });
                    tokio::time::sleep(delay).await;
                }
                Err(ClientError::Connect(detail)) => {
                    let delay = self.jittered(backoff_delay(attempt));
                    self.emit(&Event::Disconnected {
                        reason: format!("connect failed: {detail}"),
                        retry_in: delay,
                        attempt,
                    });
                    tokio::time::sleep(delay).await;
                    attempt += 1;
                }
                Err(other) => return Err(other),
            }
        }
    }

    fn jittered(&self, base: Duration) -> Duration {
        let factor = rand::rng().random_range(-self.config.jitter..self.config.jitter);
        jittered(base, factor)
    }

    fn emit(&self, event: &Event) {
        if let Some(handler) = &self.on_event {
            handler(event);
        }
    }

    /// Builds the authenticated upgrade request (fresh nonce + timestamp per
    /// attempt, SEC-03).
    fn build_request(&self) -> Result<impl IntoClientRequest, ClientError> {
        let mut request = self
            .config
            .tunnel_url
            .clone()
            .into_client_request()
            .map_err(|e| ClientError::InvalidUrl(e.to_string()))?;
        let timestamp = unix_seconds().to_string();
        let nonce = generate_nonce();
        let signature = hmac_sign(
            &self.config.secret,
            &self.config.device_id,
            &timestamp,
            &nonce,
        );

        let headers = request.headers_mut();
        let header_values = [
            (HEADER_X_DEVICE_ID, self.config.device_id.as_str()),
            (HEADER_X_DEVICE_TIMESTAMP, timestamp.as_str()),
            (HEADER_X_DEVICE_NONCE, nonce.as_str()),
            (HEADER_X_DEVICE_SIGNATURE, signature.as_str()),
        ];
        for (name, value) in header_values {
            let value = HeaderValue::from_str(value)
                .map_err(|e| ClientError::Connect(format!("invalid header {name}: {e}")))?;
            headers.insert(name, value);
        }
        Ok(request)
    }

    /// One full connection lifecycle: dial with auth headers, then serve
    /// inbound requests until the tunnel ends. Returns how it ended.
    async fn connect_and_serve(&self) -> Result<ServeEnd, ClientError> {
        let request = self.build_request()?;
        let (mut ws, _) = tokio::time::timeout(self.config.connect_timeout, connect_async(request))
            .await
            .map_err(|_| ClientError::Connect("connect timeout".into()))?
            .map_err(ClientError::Ws)?;
        self.emit(&Event::Connected);

        // Single-owner loop: reads, queued writes and heartbeat all live here
        // so frames are serialized and tungstenite pongs flush immediately.
        let (tx, mut rx) = mpsc::unbounded_channel::<Message>();
        let mut heartbeat = tokio::time::interval(self.config.heartbeat_interval);
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

        loop {
            tokio::select! {
                frame = ws.next() => {
                    match frame {
                        Some(Ok(Message::Text(text))) => {
                            if let Some(kick_reason) = self.handle_frame(text.as_str(), &tx) {
                                self.emit(&Event::Kicked { reason: Some(kick_reason.clone()) });
                                return Ok(ServeEnd::Kicked { reason: Some(kick_reason) });
                            }
                        }
                        Some(Ok(Message::Close(frame))) => {
                            let hint = frame
                                .as_ref()
                                .and_then(|f| parse_retry_hint(f.reason.as_str()));
                            return Ok(ServeEnd::Closed {
                                hint,
                                reason: frame
                                    .as_ref()
                                    .map(|f| f.reason.to_string())
                                    .unwrap_or_else(|| "no close frame".into()),
                            });
                        }
                        Some(Ok(Message::Ping(_))) => {
                            // tungstenite queues the pong; it is flushed on
                            // the next poll of this loop.
                        }
                        Some(Ok(_)) => {}
                        Some(Err(e)) => return Ok(ServeEnd::Failed { reason: e.to_string() }),
                        None => return Ok(ServeEnd::Failed { reason: "connection closed".into() }),
                    }
                }
                outbound = rx.recv() => {
                    match outbound {
                        Some(message) => {
                            if ws.send(message).await.is_err() {
                                return Ok(ServeEnd::Failed { reason: "write failed".into() });
                            }
                        }
                        None => return Ok(ServeEnd::Failed { reason: "write channel closed".into() }),
                    }
                }
                _ = heartbeat.tick() => {
                    if ws.send(heartbeat_message()).await.is_err() {
                        return Ok(ServeEnd::Failed { reason: "heartbeat write failed".into() });
                    }
                    self.emit(&Event::HeartbeatSent);
                }
            }
        }
    }

    /// Handles one inbound text frame. Returns `Some(reason)` when the server
    /// kicked this connection.
    fn handle_frame(&self, text: &str, tx: &mpsc::UnboundedSender<Message>) -> Option<String> {
        let value: serde_json::Value = match serde_json::from_str(text) {
            Ok(value) => value,
            Err(_) => return None,
        };
        // A frame with a "method" field is a request/notification; anything
        // else is a response (this SDK never awaits responses) and is ignored.
        value.get("method")?;
        let request: JSONRPCRequest = match serde_json::from_value(value) {
            Ok(request) => request,
            Err(_) => return None,
        };
        match request.method.as_str() {
            METHOD_TOOLS_LIST => {
                let defs = self.registry.tool_defs();
                let tool_count = defs.len();
                let response = JSONRPCResponse {
                    jsonrpc: "2.0".into(),
                    id: request.id.clone(),
                    result: serde_json::to_value(ToolsListResult { tools: defs }).ok(),
                    error: None,
                };
                if let Ok(payload) = serde_json::to_string(&response) {
                    let _ = tx.send(Message::text(payload));
                }
                self.emit(&Event::ToolsListServed {
                    requested_id: request.id,
                    tool_count,
                });
            }
            METHOD_TOOLS_CALL => {
                let params: ToolCallParams = serde_json::from_value(
                    request.params.unwrap_or_default(),
                )
                .unwrap_or_else(|_| ToolCallParams {
                    name: String::new(),
                    arguments: Default::default(),
                });
                let call_result = self.registry.call(&params.name, &params.arguments);
                let is_error = call_result.is_error;
                let response = JSONRPCResponse {
                    jsonrpc: "2.0".into(),
                    id: request.id.clone(),
                    result: serde_json::to_value(call_result).ok(),
                    error: None,
                };
                if let Ok(payload) = serde_json::to_string(&response) {
                    let _ = tx.send(Message::text(payload));
                }
                self.emit(&Event::ToolCallServed {
                    requested_id: request.id,
                    tool: params.name,
                    is_error,
                });
            }
            METHOD_KICK => {
                let reason = request
                    .params
                    .and_then(|p| p.get("reason").and_then(|v| v.as_str()).map(String::from));
                return Some(reason.unwrap_or_else(|| "unknown reason".to_string()));
            }
            METHOD_HEARTBEAT_ACK => {
                self.emit(&Event::Notification {
                    method: METHOD_HEARTBEAT_ACK.into(),
                });
            }
            other => {
                // Unknown request with an id gets a standard method-not-found
                // error; unknown notifications are ignored.
                if !request.id.is_empty() {
                    let response = JSONRPCResponse {
                        jsonrpc: "2.0".into(),
                        id: request.id.clone(),
                        result: None,
                        error: Some(JSONRPCError {
                            code: ERR_METHOD,
                            message: format!("method not found: {other}"),
                            data: None,
                        }),
                    };
                    if let Ok(payload) = serde_json::to_string(&response) {
                        let _ = tx.send(Message::text(payload));
                    }
                }
                self.emit(&Event::Notification {
                    method: other.to_string(),
                });
            }
        }
        None
    }
}

/// Builds the application-level `heartbeat` notification (design/33 3.3).
fn heartbeat_message() -> Message {
    let body = serde_json::json!({
        "jsonrpc": "2.0",
        "method": METHOD_HEARTBEAT,
        "params": {
            "sdk_version": env!("CARGO_PKG_VERSION"),
            "sent_at": rfc3339_utc(unix_seconds()),
        }
    });
    Message::text(body.to_string())
}

/// Extracts `retry_after_secs` from a JSON close-frame reason, if present
/// (design/31 3.1.8).
fn parse_retry_hint(reason: &str) -> Option<u64> {
    let value: serde_json::Value = serde_json::from_str(reason).ok()?;
    value.get("retry_after_secs")?.as_u64()
}

fn unix_seconds() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Formats a unix timestamp as RFC 3339 UTC without external chrono
/// dependency (civil-from-days algorithm).
fn rfc3339_utc(secs: i64) -> String {
    let days = secs.div_euclid(86_400);
    let rem = secs.rem_euclid(86_400);
    let (hour, minute, second) = (rem / 3600, (rem % 3600) / 60, rem % 60);

    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let mut year = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    if month <= 2 {
        year += 1;
    }
    format!("{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}Z")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_ladder_matches_lld_3_1_8() {
        let expected: [u64; 10] = [1, 2, 4, 8, 16, 30, 60, 60, 60, 60];
        for (attempt, secs) in expected.iter().enumerate() {
            assert_eq!(
                backoff_delay(attempt as u32),
                Duration::from_secs(*secs),
                "attempt {attempt}"
            );
        }
    }

    #[test]
    fn jitter_stays_within_amplitude() {
        let base = Duration::from_secs(10);
        let upper = jittered(base, 0.2);
        let lower = jittered(base, -0.2);
        assert_eq!(upper, Duration::from_secs_f64(12.0));
        assert_eq!(lower, Duration::from_secs_f64(8.0));
        assert_eq!(jittered(base, -1.0), Duration::from_secs(0));
    }

    #[test]
    fn retry_hint_parsed_from_json_close_reason() {
        assert_eq!(parse_retry_hint(r#"{"retry_after_secs": 7}"#), Some(7));
        assert_eq!(parse_retry_hint("going away"), None);
        assert_eq!(parse_retry_hint(""), None);
    }

    #[test]
    fn rfc3339_known_timestamp() {
        // Verified against Python datetime.fromtimestamp(..., tz=utc).
        assert_eq!(rfc3339_utc(1_755_117_000), "2025-08-13T20:30:00Z");
        assert_eq!(rfc3339_utc(1_786_696_200), "2026-08-14T08:30:00Z");
        assert_eq!(rfc3339_utc(0), "1970-01-01T00:00:00Z");
    }
}
