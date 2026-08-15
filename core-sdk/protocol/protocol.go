// Package protocol 定义 ADC 设备面 wire 协议（JSON-RPC 2.0 风格，MCP 语义）。
// 属于 core-sdk 仓库（Apache-2.0），SDK 与云端共用，禁止引入任何外部依赖。
package protocol

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC 2.0 基础报文

type JSONRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Error 实现 error 接口，便于网关层统一包装。
func (e *JSONRPCError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// 标准 JSON-RPC 错误码
const (
	ErrParse      = -32700
	ErrInvalidReq = -32600
	ErrMethod     = -32601
	ErrInvalidArg = -32602
	ErrInternal   = -32603
)

// MCP 工具定义

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// RiskLevel 设备上报的建议风险等级（0-3）；云端 DB 为权威，仅作建议（SEC-09）。
	RiskLevel *int `json:"riskLevel,omitempty"`
	// SchemaVersion 工具 schema 版本，用于兼容矩阵（SEC-22）。
	SchemaVersion string `json:"schemaVersion,omitempty"`
}

func (t MCPTool) EffectiveRisk() int {
	if t.RiskLevel != nil {
		return *t.RiskLevel
	}
	return 2 // 缺省先审后用
}

type ToolsListResult struct {
	Tools []MCPTool `json:"tools"`
}

type ToolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

type ToolCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// 方法名常量
const (
	MethodToolsList = "tools/list"
	MethodToolsCall = "tools/call"
)

// 设备面鉴权握手常量（SEC-03）
const (
	// HeaderXDeviceID 设备码头
	HeaderXDeviceID = "X-Device-ID"
	// HeaderXDeviceTimestamp 认证时间戳（Unix 秒）
	HeaderXDeviceTimestamp = "X-Device-Timestamp"
	// HeaderXDeviceNonce 一次性 nonce
	HeaderXDeviceNonce = "X-Device-Nonce"
	// HeaderXDeviceSignature HMAC(secret, deviceID + timestamp + nonce)
	HeaderXDeviceSignature = "X-Device-Signature"
	// AuthTimeWindowSec 时间戳漂移窗口
	AuthTimeWindowSec = 300
)
