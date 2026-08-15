// Package httpx 提供 HTTP 服务公共设施：统一响应、错误结构与中间件装配。
package httpx

import (
	"encoding/json"
	"net/http"
)

// ErrorBody 统一错误结构（design/33：code/message/trace_id）。
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"`
}

// WriteError 输出统一错误 JSON。
func WriteError(w http.ResponseWriter, status int, code, msg, traceID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Code: code, Message: msg, TraceID: traceID})
}

// WriteJSON 输出成功 JSON。
func WriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TraceIDFrom 从 context 或 header 读取 trace_id（审计贯通）。
// 优先取 TraceID 中间件写入的 context 值，其次 X-Trace-ID，最后 X-Request-ID。
func TraceIDFrom(r *http.Request) string {
	if v := TraceIDFromCtx(r.Context()); v != "" {
		return v
	}
	if v := r.Header.Get(HeaderTraceID); v != "" {
		return v
	}
	return r.Header.Get("X-Request-ID")
}
