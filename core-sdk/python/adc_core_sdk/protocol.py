"""Device-plane wire protocol types for ADC (JSON-RPC 2.0 style, MCP semantics).

This module mirrors core-sdk/protocol/protocol.go field by field: JSON field
names, required/optional semantics and defaults must stay identical to the Go
implementation so both languages speak the same wire contract. Contract
fidelity is pinned by the hardcoded sample payloads in tests/test_protocol.py.
"""

from __future__ import annotations

import json
from typing import Any

from pydantic import BaseModel


class WireModel(BaseModel):
    """Base class reproducing Go encoding/json omitempty behaviour on dump.

    Go omits nil pointers, nil maps/interfaces, empty omitempty strings and
    empty omitempty slices. Python models mirror this by excluding None
    values, any optional string fields listed in ``_omitempty_str_fields``
    when their value is "" and any list fields listed in
    ``_omitempty_list_fields`` when their value is empty.
    """

    #: Optional string fields that Go marshals with omitempty.
    _omitempty_str_fields: tuple[str, ...] = ()
    #: Optional list fields that Go marshals with omitempty (len == 0).
    _omitempty_list_fields: tuple[str, ...] = ()

    def to_wire_dict(self) -> dict[str, Any]:
        """Return the wire-compatible dict with Go omitempty semantics applied."""
        data: dict[str, Any] = {}
        for field_name in self.__class__.model_fields:
            value: Any = getattr(self, field_name)
            if isinstance(value, WireModel):
                value = value.to_wire_dict()
            elif isinstance(value, list):
                value = [
                    item.to_wire_dict() if isinstance(item, WireModel) else item for item in value
                ]
            if value is None:
                continue
            if field_name in self._omitempty_str_fields and value == "":
                continue
            if (
                field_name in self._omitempty_list_fields
                and isinstance(value, list)
                and len(value) == 0
            ):
                continue
            data[field_name] = value
        return data

    def to_wire_json(self) -> str:
        """Return compact wire JSON (same shape as Go json.Marshal output)."""
        return json.dumps(self.to_wire_dict(), separators=(",", ":"), ensure_ascii=False)


# ---------------------------------------------------------------------------
# JSON-RPC 2.0 base envelopes
# ---------------------------------------------------------------------------


class JSONRPCError(WireModel):
    code: int
    message: str
    data: Any | None = None

    def __str__(self) -> str:
        return f"jsonrpc error {self.code}: {self.message}"


class JSONRPCRequest(WireModel):
    jsonrpc: str
    id: str
    method: str
    params: Any | None = None
    #: Wire protocol version (GAP-06); absent means 1.0.
    version: str | None = None

    _omitempty_str_fields = ("version",)

    @property
    def effective_version(self) -> str:
        """Return the request version, defaulting to 1.0 when absent."""
        return self.version or PROTOCOL_VERSION


class JSONRPCResponse(WireModel):
    jsonrpc: str
    id: str
    result: Any | None = None
    error: JSONRPCError | None = None


# Standard JSON-RPC error codes (identical to protocol.go).
ERR_PARSE = -32700
ERR_INVALID_REQUEST = -32600
ERR_METHOD_NOT_FOUND = -32601
ERR_INVALID_PARAMS = -32602
ERR_INTERNAL = -32603

# ---------------------------------------------------------------------------
# Protocol version negotiation (GAP-06, identical to protocol.go)
# ---------------------------------------------------------------------------

#: Wire protocol version this SDK speaks; requests or handshakes without a
#: "version"/"protocolVersion" field are treated as 1.0.
PROTOCOL_VERSION = "1.0"

#: Returned when a peer advertises a protocol version we cannot speak.
#: JSON-RPC 2.0 reserves the -32000..-32099 range for server-defined errors.
ERR_VERSION_UNSUPPORTED = -32001


def negotiate_version(version: str | None) -> JSONRPCError | None:
    """Validate a peer-advertised version; empty/None means 1.0 (backward compat).

    Mirrors NegotiateVersion in protocol.go: any value other than "" and
    PROTOCOL_VERSION yields an error with code ERR_VERSION_UNSUPPORTED.
    """
    if version is None or version == "" or version == PROTOCOL_VERSION:
        return None
    return JSONRPCError(
        code=ERR_VERSION_UNSUPPORTED,
        message=f"unsupported protocol version: {version}",
    )


class Handshake(WireModel):
    """Connection-opening message a device sends after transport setup (GAP-06)."""

    protocolVersion: str
    capabilities: list[str] | None = None

    _omitempty_list_fields = ("capabilities",)


# ---------------------------------------------------------------------------
# MCP tool definitions
# ---------------------------------------------------------------------------

#: Default risk level when a device does not report one: approve before use.
DEFAULT_RISK_LEVEL = 2


class MCPTool(WireModel):
    name: str
    description: str | None = None
    inputSchema: Any
    # RiskLevel is the device-reported suggestion (0-3); the cloud DB is
    # authoritative and this value is advisory only (SEC-09).
    riskLevel: int | None = None
    # SchemaVersion is used by the compatibility matrix (SEC-22).
    schemaVersion: str | None = None

    _omitempty_str_fields = ("description", "schemaVersion")

    @property
    def effective_risk(self) -> int:
        """Return the effective risk level, defaulting to 2 when absent."""
        return self.riskLevel if self.riskLevel is not None else DEFAULT_RISK_LEVEL


class ToolsListResult(WireModel):
    tools: list[MCPTool]


class ToolCallParams(WireModel):
    name: str
    arguments: dict[str, Any] | None = None


class ToolContent(WireModel):
    type: str
    text: str | None = None

    _omitempty_str_fields = ("text",)


class ToolCallResult(WireModel):
    content: list[ToolContent]
    isError: bool


# ---------------------------------------------------------------------------
# Method name constants (identical to protocol.go)
# ---------------------------------------------------------------------------

METHOD_TOOLS_LIST = "tools/list"
METHOD_TOOLS_CALL = "tools/call"

# ---------------------------------------------------------------------------
# Device-plane auth handshake constants (SEC-03, identical to protocol.go)
# ---------------------------------------------------------------------------

HEADER_X_DEVICE_ID = "X-Device-ID"
HEADER_X_DEVICE_TIMESTAMP = "X-Device-Timestamp"
HEADER_X_DEVICE_NONCE = "X-Device-Nonce"
HEADER_X_DEVICE_SIGNATURE = "X-Device-Signature"
AUTH_TIME_WINDOW_SEC = 300
