"""Dual-language wire contract tests (design/80 E-01).

Every sample payload below is hardcoded. JSON field names are pinned to the
Go struct tags in core-sdk/protocol/protocol.go (noted inline per test); the
Go twin of this file is core-sdk/protocol/protocol_test.go. If any test here
fails, one of the two languages has drifted from the frozen wire contract.
"""

from __future__ import annotations

import json

from adc_core_sdk import (
    AUTH_TIME_WINDOW_SEC,
    ERR_INVALID_PARAMS,
    ERR_VERSION_UNSUPPORTED,
    HEADER_X_DEVICE_ID,
    HEADER_X_DEVICE_NONCE,
    HEADER_X_DEVICE_SIGNATURE,
    HEADER_X_DEVICE_TIMESTAMP,
    PROTOCOL_VERSION,
    Handshake,
    JSONRPCError,
    JSONRPCRequest,
    JSONRPCResponse,
    ToolCallParams,
    ToolCallResult,
    ToolContent,
    negotiate_version,
)


def test_device_auth_header_constants_match_go() -> None:
    # Go: HeaderXDeviceID = "X-Device-ID" (json tag not applicable, raw
    # header names frozen in protocol.go "设备面鉴权握手常量" block).
    assert HEADER_X_DEVICE_ID == "X-Device-ID"
    assert HEADER_X_DEVICE_TIMESTAMP == "X-Device-Timestamp"
    assert HEADER_X_DEVICE_NONCE == "X-Device-Nonce"
    assert HEADER_X_DEVICE_SIGNATURE == "X-Device-Signature"
    assert AUTH_TIME_WINDOW_SEC == 300


def test_handshake_wire_shape_matches_go() -> None:
    # Go: type Handshake struct {
    #   ProtocolVersion string   `json:"protocolVersion"`
    #   Capabilities    []string `json:"capabilities,omitempty"`
    # }
    sample = {"protocolVersion": "1.0", "capabilities": ["tools", "handshake"]}
    handshake = Handshake(protocolVersion="1.0", capabilities=["tools", "handshake"])
    assert handshake.to_wire_dict() == sample
    assert Handshake.model_validate(sample).to_wire_dict() == sample


def test_handshake_omits_empty_capabilities_like_go_omitempty() -> None:
    # Go omitempty drops nil/empty capabilities; Python mirrors via
    # _omitempty_list_fields.
    assert Handshake(protocolVersion="1.0").to_wire_dict() == {"protocolVersion": "1.0"}
    assert Handshake(protocolVersion="1.0", capabilities=[]).to_wire_dict() == {
        "protocolVersion": "1.0"
    }


def test_protocol_version_constant_and_error_code_match_go() -> None:
    # Go: const ProtocolVersion = "1.0"; const ErrVersionUnsupported = -32001
    assert PROTOCOL_VERSION == "1.0"
    assert ERR_VERSION_UNSUPPORTED == -32001


def test_negotiate_version_backward_compat_matches_go() -> None:
    # Go: NegotiateVersion("") == nil (missing version treated as 1.0)
    assert negotiate_version(None) is None
    assert negotiate_version("") is None
    assert negotiate_version("1.0") is None
    for bad in ("0.9", "2.0"):
        err = negotiate_version(bad)
        assert err is not None
        assert err.code == -32001
        assert err.message == f"unsupported protocol version: {bad}"


def test_request_with_version_matches_go() -> None:
    # Go: Version string `json:"version,omitempty"` added to JSONRPCRequest
    sample = {"jsonrpc": "2.0", "id": "req-1", "method": "tools/list", "version": "1.0"}
    request = JSONRPCRequest(jsonrpc="2.0", id="req-1", method="tools/list", version="1.0")
    assert request.to_wire_dict() == sample
    assert JSONRPCRequest.model_validate(sample).to_wire_dict() == sample


def test_request_without_version_defaults_to_1_0() -> None:
    # Go: EffectiveVersion() returns ProtocolVersion when Version == ""
    sample = {"jsonrpc": "2.0", "id": "req-1", "method": "tools/list"}
    request = JSONRPCRequest.model_validate(sample)
    assert request.version is None
    assert request.effective_version == "1.0"
    assert request.to_wire_dict() == sample  # "version" stays omitted on dump


def test_tools_list_request_matches_go() -> None:
    # Go: JSONRPCRequest with Method = MethodToolsList ("tools/list")
    sample = {"jsonrpc": "2.0", "id": "req-1", "method": "tools/list", "version": "1.0"}
    request = JSONRPCRequest.model_validate(sample)
    assert request.method == "tools/list"
    assert json.loads(request.to_wire_json()) == sample


def test_tools_list_response_matches_go() -> None:
    # Go json tags: name, description,omitempty; inputSchema; riskLevel,omitempty;
    # schemaVersion,omitempty inside `tools`.
    sample = {
        "jsonrpc": "2.0",
        "id": "req-1",
        "result": {
            "tools": [
                {
                    "name": "dev-01::reboot",
                    "description": "Reboot the device",
                    "inputSchema": {
                        "type": "object",
                        "properties": {"delay_ms": {"type": "integer"}},
                    },
                    "riskLevel": 3,
                    "schemaVersion": "v1",
                }
            ]
        },
    }
    parsed = JSONRPCResponse.model_validate(sample)
    assert parsed.to_wire_dict() == sample
    assert parsed.error is None


def test_tools_call_request_and_result_match_go() -> None:
    # Go: ToolCallParams json tags: name, arguments,omitempty;
    # ToolCallResult: content, isError; ToolContent: type, text,omitempty.
    request_sample = {
        "jsonrpc": "2.0",
        "id": "req-2",
        "method": "tools/call",
        "params": {"name": "dev-01::reboot", "arguments": {"delay_ms": 500}},
    }
    request = JSONRPCRequest(
        jsonrpc="2.0",
        id="req-2",
        method="tools/call",
        params=ToolCallParams(name="dev-01::reboot", arguments={"delay_ms": 500}),
    )
    assert request.to_wire_dict() == request_sample

    result_sample = {
        "content": [{"type": "text", "text": "reboot scheduled"}, {"type": "text"}],
        "isError": False,
    }
    result = ToolCallResult(
        content=[ToolContent(type="text", text="reboot scheduled"), ToolContent(type="text")],
        isError=False,
    )
    assert result.to_wire_dict() == result_sample
    assert ToolCallResult.model_validate(result_sample).to_wire_dict() == result_sample


def test_error_response_matches_go() -> None:
    # Go: JSONRPCError json tags: code, message, data,omitempty.
    sample = {
        "jsonrpc": "2.0",
        "id": "req-3",
        "error": {"code": -32602, "message": "Invalid params", "data": {"field": "name"}},
    }
    response = JSONRPCResponse(
        jsonrpc="2.0",
        id="req-3",
        error=JSONRPCError(
            code=ERR_INVALID_PARAMS,
            message="Invalid params",
            data={"field": "name"},
        ),
    )
    assert response.to_wire_dict() == sample
    assert JSONRPCResponse.model_validate(sample).to_wire_dict() == sample


def test_version_unsupported_error_response_matches_go() -> None:
    # Go: NegotiateVersion("0.9") -> &JSONRPCError{Code: -32001,
    # Message: "unsupported protocol version: 0.9"}
    err = negotiate_version("0.9")
    assert err is not None
    response = JSONRPCResponse(jsonrpc="2.0", id="hs-1", error=err)
    sample = {
        "jsonrpc": "2.0",
        "id": "hs-1",
        "error": {"code": -32001, "message": "unsupported protocol version: 0.9"},
    }
    assert response.to_wire_dict() == sample
    assert JSONRPCResponse.model_validate(sample).to_wire_dict() == sample
