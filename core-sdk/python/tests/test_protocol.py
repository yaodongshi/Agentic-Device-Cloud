"""Wire-contract tests for the Python protocol types.

Sample payloads below are pinned against the Go implementation
(core-sdk/protocol/protocol.go): field names, omission rules and defaults
must never drift between the two languages.
"""

from __future__ import annotations

import json

from adc_core_sdk import (
    DEFAULT_RISK_LEVEL,
    ERR_INTERNAL,
    ERR_INVALID_PARAMS,
    ERR_INVALID_REQUEST,
    ERR_METHOD_NOT_FOUND,
    ERR_PARSE,
    METHOD_TOOLS_CALL,
    METHOD_TOOLS_LIST,
    JSONRPCError,
    JSONRPCRequest,
    JSONRPCResponse,
    MCPTool,
    ToolCallParams,
    ToolCallResult,
    ToolContent,
    ToolsListResult,
)


def test_constants_match_go() -> None:
    assert METHOD_TOOLS_LIST == "tools/list"
    assert METHOD_TOOLS_CALL == "tools/call"
    assert DEFAULT_RISK_LEVEL == 2
    assert (
        ERR_PARSE,
        ERR_INVALID_REQUEST,
        ERR_METHOD_NOT_FOUND,
        ERR_INVALID_PARAMS,
        ERR_INTERNAL,
    ) == (-32700, -32600, -32601, -32602, -32603)


def test_request_minimal_omits_params() -> None:
    request = JSONRPCRequest(jsonrpc="2.0", id="req-1", method="tools/list")
    assert request.to_wire_dict() == {"jsonrpc": "2.0", "id": "req-1", "method": "tools/list"}


def test_request_with_call_params_matches_go_sample() -> None:
    sample = {
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
    assert request.to_wire_dict() == sample
    assert JSONRPCRequest.model_validate(sample).params == {
        "name": "dev-01::reboot",
        "arguments": {"delay_ms": 500},
    }


def test_tool_call_params_omits_empty_arguments() -> None:
    assert ToolCallParams(name="dev-01::ping").to_wire_dict() == {"name": "dev-01::ping"}


def test_response_with_tools_list_result_matches_go_sample() -> None:
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
                },
                {"name": "dev-01::get_telemetry", "inputSchema": {"type": "object"}},
            ]
        },
    }
    tools = ToolsListResult(
        tools=[
            MCPTool(
                name="dev-01::reboot",
                description="Reboot the device",
                inputSchema={
                    "type": "object",
                    "properties": {"delay_ms": {"type": "integer"}},
                },
                riskLevel=3,
                schemaVersion="v1",
            ),
            MCPTool(name="dev-01::get_telemetry", inputSchema={"type": "object"}),
        ]
    )
    response = JSONRPCResponse(jsonrpc="2.0", id="req-1", result=tools)
    assert response.to_wire_dict() == sample
    assert JSONRPCResponse.model_validate(sample).to_wire_dict() == sample


def test_response_with_error_matches_go_sample() -> None:
    sample = {
        "jsonrpc": "2.0",
        "id": "req-3",
        "error": {"code": -32602, "message": "Invalid params", "data": {"field": "name"}},
    }
    response = JSONRPCResponse(
        jsonrpc="2.0",
        id="req-3",
        error=JSONRPCError(code=-32602, message="Invalid params", data={"field": "name"}),
    )
    assert response.to_wire_dict() == sample


def test_jsonrpc_error_str_matches_go_error_interface() -> None:
    assert str(JSONRPCError(code=-32602, message="Invalid params")) == (
        "jsonrpc error -32602: Invalid params"
    )


def test_mcp_tool_effective_risk_defaults_to_2() -> None:
    assert MCPTool(name="t", inputSchema={"type": "object"}).effective_risk == 2
    assert MCPTool(name="t", inputSchema={"type": "object"}, riskLevel=0).effective_risk == 0
    assert MCPTool(name="t", inputSchema={"type": "object"}, riskLevel=3).effective_risk == 3


def test_mcp_tool_omits_empty_description() -> None:
    tool = MCPTool(name="t", description="", inputSchema={"type": "object"})
    assert tool.to_wire_dict() == {"name": "t", "inputSchema": {"type": "object"}}


def test_tool_call_result_matches_go_sample() -> None:
    sample = {
        "content": [{"type": "text", "text": "reboot scheduled"}, {"type": "text"}],
        "isError": False,
    }
    result = ToolCallResult(
        content=[
            ToolContent(type="text", text="reboot scheduled"),
            ToolContent(type="text"),
        ],
        isError=False,
    )
    assert result.to_wire_dict() == sample
    parsed = ToolCallResult.model_validate(sample)
    assert parsed.isError is False
    assert parsed.content[1].text is None


def test_wire_json_is_compact_and_valid() -> None:
    request = JSONRPCRequest(jsonrpc="2.0", id="1", method="tools/list")
    expected = {"jsonrpc": "2.0", "id": "1", "method": "tools/list"}
    assert json.loads(request.to_wire_json()) == expected


def test_unknown_fields_are_ignored_like_go() -> None:
    parsed = MCPTool.model_validate({"name": "t", "inputSchema": {}, "unknownField": 42})
    assert parsed.to_wire_dict() == {"name": "t", "inputSchema": {}}
