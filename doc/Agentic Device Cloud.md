
# Agentic Device Cloud (ADC) 技术架构与系统实现方案

**Agentic Device Cloud (ADC)** 是一座面向工业级与高端智能场景的 **AI 原生设备编排与治理平台**。它将物理硬件抽象为具备标准 MCP（Model Context Protocol）契约的“智能工具节点”，同时为上层 Agent 提供受控的 LLM 计算接入与跨设备协同中枢。

---

## 1. 业务与系统总体架构

### 1.1 核心设计理念

传统 IoT 平台专注于“数据单向采集与静态规则流转”，将硬件视为被动的哑终端。ADC 则构建“意图理解、主动探测、动态推理与受控执行”的 AI 原生设备拓扑：

* **设备即 MCP 节点**：硬件（或边缘盒子）内嵌 MCP 运行时，开机主动向云端注册自身具备的工具链（`tools/list`）。
* **NAT 反向穿透**：硬件通过 WSS 长连接反向接入云端，解决工业内网/家庭网络无公网 IP 的寻址痛点。
* **统一虚拟聚合**：网关动态提取所有在线设备能力，自动附加命名空间并合并为一个全局虚拟 MCP 端点供 Agent 调度。
* **工业级安全防线**：内置大模型网关与 Human-in-the-Loop（HITL）人工审批熔断机制，杜绝模型幻觉导致实体硬件误动作。

---

### 1.2 工业协同闭环拓扑

```text
                    ┌───────────────────────────────┐
                    │     用户 / 工业协同 Agent     │
                    └──────────────┬────────────────┘
                                   │ 1. 自然语言意图 / 业务目标
                                   ▼
┌────────────────────────────────────────────────────────────────────────┐
│                   Agentic Device Cloud (平台中枢)                       │
│                                                                        │
│   ┌────────────────────┐ 2. 意图解析  ┌────────────────────────────┐    │
│   │ 统一 LLM 网关中枢  │ ◄────────── │ 动态 MCP 工具聚合与路由   │    │
│   │ (算力调度/Token管控)│ ──────────► │ (设备发现/上下文增强)     │    │
│   └────────────────────┘ 3. 决策规划  └─────────────┬──────────────┘    │
│                                                     │ 4. 权限校验/HITL   │
└─────────────────────────────────────────────────────┼──────────────────┘
                                                      │ 5. 穿透 NAT 下发
                                                      ▼
                                       ┌─────────────────────────────┐
                                       │ 现场硬件 / 工业边缘节点     │
                                       │ (Embedded MCP Server 运行时)│
                                       └─────────────────────────────┘

```

---

### 1.3 核心业务场景

* **预测性自治维护**：设备发生异常振动 $\rightarrow$ 边缘 MCP 上报异常上下文 $\rightarrow$ 平台 Agent 自动调用该设备及周边环境的 MCP 诊断工具 $\rightarrow$ 生成排查结论并请求工程师确认。
* **柔性产线排产重构**：产线调度 Agent 遍历租户下所有在线 CNC 机床的 MCP 规格与负载 $\rightarrow$ 自动分配加工指令并同步仓储 AGV。
* **工业安全双重防线**：任何涉及断电、急停、工艺参数覆盖的高危 MCP Tool 调用，强制触发平台级 **Human-in-the-Loop (HITL)** 审批流程。

---

## 2. 系统分层与开源基础设施底座

| 平台分层                 | 核心职责                                         | 开源技术选型 / 核心参考                      |
| ------------------------ | ------------------------------------------------ | -------------------------------------------- |
| **设备边缘连接层** | 边缘硬件反向穿透、双向安全信道维护、工具动态发现 | •`agentic-community/mcp-gateway-registry` |

• `gorilla/websocket` / `gRPC-Web`

• `nats-io/nats-server` |
| **模型网关与调度层** | 聚合多模型供应商、Token 计量、故障转移、Prompt 上下文注入 | • `BerriAI/litellm` 或 `songquanpeng/one-api`

• `langfuse/langfuse` (可观测性与链路追踪) |
| **安全与访问控制层** | 设备身份凭证（mTLS/HMAC）、RBAC/ABAC、细粒度 Tool 拦截 | • `casbin/casbin` (多租户权限引擎)

• `open-policy-agent/opa` (策略决策) |
| **持久化与状态存储** | 租户配置、设备元数据、操作审计日志、工具缓存 | • **PostgreSQL**：租户关系型数据与配置

• **Redis**：设备在线心跳、工具会话上下文、分布式锁 |

---

## 3. 数据库模型设计 (PostgreSQL Schema)

```sql
-- 1. 设备注册与认证表
CREATE TABLE adc_devices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    device_code VARCHAR(128) NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    device_type VARCHAR(64) NOT NULL, -- e.g., 'plc', 'cnc', 'sensor_hub'
    auth_type VARCHAR(32) NOT NULL,   -- 'token', 'hmac', 'mtls'
    auth_secret_hash TEXT NOT NULL,
    status VARCHAR(32) DEFAULT 'offline', -- 'online', 'offline', 'error'
    last_heartbeat TIMESTAMPTZ,
    metadata JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- 2. 动态 MCP 工具缓存表 (由设备注册后上报自动生成)
CREATE TABLE adc_device_tools (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id UUID REFERENCES adc_devices(id) ON DELETE CASCADE,
    tool_name VARCHAR(128) NOT NULL,
    description TEXT,
    input_schema JSONB NOT NULL,
    risk_level INT DEFAULT 0,         -- 0: Read, 1: Low, 2: High (HITL), 3: Critical
    is_enabled BOOLEAN DEFAULT TRUE,
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(device_id, tool_name)
);

-- 3. 审计与调用链路追踪表
CREATE TABLE adc_mcp_call_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    agent_id VARCHAR(64) NOT NULL,
    device_id UUID REFERENCES adc_devices(id),
    tool_name VARCHAR(128) NOT NULL,
    request_params JSONB NOT NULL,
    response_payload JSONB,
    execution_duration_ms INT,
    status VARCHAR(32) NOT NULL,      -- 'success', 'failed', 'blocked_by_hitl'
    hitl_approver VARCHAR(64),
    created_at TIMESTAMPTZ DEFAULT NOW()
);

```

---

## 4. 核心反向 WebSocket 网关实现 (Go 语言)

### 4.1 协议定义 (`protocol/mcp.go`)

```go
package protocol

import "encoding/json"

// JSON-RPC 2.0 基础报文规范
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

// MCP 协议核心定义
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
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

```

---

### 4.2 设备鉴权引擎 (`auth/authenticator.go`)

```go
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
)

type DeviceCredential struct {
	DeviceID string
	TenantID string
	Secret   string
	Enabled  bool
}

type Authenticator struct {
	mu      sync.RWMutex
	devices map[string]DeviceCredential
}

func NewAuthenticator() *Authenticator {
	auth := &Authenticator{
		devices: make(map[string]DeviceCredential),
	}
	// 预置设备凭证 (生产环境对接 DB/Vault)
	auth.devices["cnc-lathe-01"] = DeviceCredential{
		DeviceID: "cnc-lathe-01",
		TenantID: "tenant-factory-01",
		Secret:   "sec_cnc_secret_key_8899",
		Enabled:  true,
	}
	auth.devices["temp-sensor-02"] = DeviceCredential{
		DeviceID: "temp-sensor-02",
		TenantID: "tenant-factory-01",
		Secret:   "sec_sensor_key_1122",
		Enabled:  true,
	}
	return auth
}

func (a *Authenticator) Verify(deviceID, token string) (*DeviceCredential, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	cred, exists := a.devices[deviceID]
	if !exists || !cred.Enabled {
		return nil, errors.New("device not registered or disabled")
	}

	// 1. 静态 Token 匹配
	if token == cred.Secret {
		return &cred, nil
	}

	// 2. HMAC-SHA256 签名匹配 (防重放)
	h := hmac.New(sha256.New, []byte(cred.Secret))
	h.Write([]byte(deviceID))
	if token == hex.EncodeToString(h.Sum(nil)) {
		return &cred, nil
	}

	return nil, errors.New("invalid signature / token")
}

```

---

### 4.3 设备会话与生命周期 (`hub/session.go`)

```go
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"adc-gateway/protocol"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512 * 1024
)

type DeviceSession struct {
	DeviceID string
	TenantID string
	Conn     *websocket.Conn

	writeMu    sync.Mutex
	toolsMu    sync.RWMutex
	Tools      []protocol.MCPTool
	pendingMu  sync.Mutex
	pendingReq map[string]chan *protocol.JSONRPCResponse
	closeOnce  sync.Once
	doneChan   chan struct{}
}

func NewDeviceSession(deviceID, tenantID string, conn *websocket.Conn) *DeviceSession {
	return &DeviceSession{
		DeviceID:   deviceID,
		TenantID:   tenantID,
		Conn:       conn,
		Tools:      make([]protocol.MCPTool, 0),
		pendingReq: make(map[string]chan *protocol.JSONRPCResponse),
		doneChan:   make(chan struct{}),
	}
}

func (s *DeviceSession) SendRPC(ctx context.Context, method string, params interface{}) (*protocol.JSONRPCResponse, error) {
	select {
	case <-s.doneChan:
		return nil, errors.New("device session closed")
	default:
	}

	reqID := uuid.New().String()
	req := protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  method,
		Params:  params,
	}

	respChan := make(chan *protocol.JSONRPCResponse, 1)

	s.pendingMu.Lock()
	s.pendingReq[reqID] = respChan
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pendingReq, reqID)
		s.pendingMu.Unlock()
	}()

	s.writeMu.Lock()
	s.Conn.SetWriteDeadline(time.Now().Add(writeWait))
	err := s.Conn.WriteJSON(req)
	s.writeMu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("write rpc request failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.doneChan:
		return nil, errors.New("connection closed while waiting for response")
	case resp := <-respChan:
		return resp, nil
	}
}

func (s *DeviceSession) StartPump(onClose func()) {
	defer func() {
		s.closeOnce.Do(func() {
			close(s.doneChan)
			s.Conn.Close()
			onClose()
		})
	}()

	s.Conn.SetReadLimit(maxMessageSize)
	s.Conn.SetReadDeadline(time.Now().Add(pongWait))
	s.Conn.SetPongHandler(func(string) error {
		s.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	go s.heartbeatLoop()

	for {
		_, msg, err := s.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[Device:%s] Error: %v", s.DeviceID, err)
			}
			break
		}
		s.dispatchMessage(msg)
	}
}

func (s *DeviceSession) heartbeatLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-s.doneChan:
			return
		case <-ticker.C:
			s.writeMu.Lock()
			s.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := s.Conn.WriteMessage(websocket.PingMessage, nil)
			s.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (s *DeviceSession) dispatchMessage(msg []byte) {
	var resp protocol.JSONRPCResponse
	if err := json.Unmarshal(msg, &resp); err == nil && resp.ID != "" {
		s.pendingMu.Lock()
		ch, exists := s.pendingReq[resp.ID]
		s.pendingMu.Unlock()

		if exists {
			ch <- &resp
		}
	}
}

func (s *DeviceSession) SetTools(tools []protocol.MCPTool) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	s.Tools = tools
}

func (s *DeviceSession) GetTools() []protocol.MCPTool {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return s.Tools
}

```

---

### 4.4 本地路由表中心 (`hub/device_hub.go`)

```go
package hub

import (
	"errors"
	"fmt"
	"sync"
)

type DeviceHub struct {
	mu      sync.RWMutex
	devices map[string]*DeviceSession // key: tenantID:deviceID
}

func NewDeviceHub() *DeviceHub {
	return &DeviceHub{
		devices: make(map[string]*DeviceSession),
	}
}

func (h *DeviceHub) key(tenantID, deviceID string) string {
	return fmt.Sprintf("%s:%s", tenantID, deviceID)
}

func (h *DeviceHub) Register(session *DeviceSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.devices[h.key(session.TenantID, session.DeviceID)] = session
}

func (h *DeviceHub) Unregister(tenantID, deviceID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.devices, h.key(tenantID, deviceID))
}

func (h *DeviceHub) GetDevice(tenantID, deviceID string) (*DeviceSession, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	session, exists := h.devices[h.key(tenantID, deviceID)]
	if !exists {
		return nil, errors.New("device is offline or not found")
	}
	return session, nil
}

func (h *DeviceHub) ListDevicesByTenant(tenantID string) []*DeviceSession {
	h.mu.RLock()
	defer h.mu.RUnlock()

	list := make([]*DeviceSession, 0)
	for _, dev := range h.devices {
		if dev.TenantID == tenantID {
			list = append(list, dev)
		}
	}
	return list
}

```

---

## 5. 基于 Redis Pub/Sub 的多实例集群扩展

在多实例集群中，设备连接分散在各个网关实例上。通过 Redis 维护状态共享，并利用 Pub/Sub 进行跨节点 RPC 穿透调度：

```text
[Agent 请求] ──► [ 网关实例 A (Node-A) ]
                        │
                        ├─ 1. 查询 Redis: 设备连在哪个节点？ (查得: Node-B)
                        │
                        ├─ 2. Pub/Sub 发送指令到通道: adc:req:Node-B
                        ▼
                 [ Redis 集群 / Pub/Sub ]
                        │
                        ▼
                 [ 网关实例 B (Node-B) ]
                        │
                        ├─ 3. 提取本地 WebSocket 会话，下发 JSON-RPC 2.0
                        ▼
                 [ 物理硬件 (内网 WSS) ]
                        │
                        ├─ 4. 设备响应结果
                        ▼
                 [ 网关实例 B (Node-B) ]
                        │
                        ├─ 5. Pub/Sub 将结果投递回通道: adc:resp:Node-A
                        ▼
[Agent 获得响应] ◄── [ 网关实例 A (Node-A) ]

```

---

### 5.1 跨节点通信协议 (`protocol/cluster.go`)

```go
package protocol

import "encoding/json"

type ClusterCallRequest struct {
	RequestID  string                 `json:"request_id"`
	FromNodeID string                 `json:"from_node_id"`
	TenantID   string                 `json:"tenant_id"`
	DeviceID   string                 `json:"device_id"`
	ToolName   string                 `json:"tool_name"`
	Arguments  map[string]interface{} `json:"arguments"`
}

type ClusterCallResponse struct {
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *JSONRPCError   `json:"error,omitempty"`
}

```

---

### 5.2 集群管理器 (`cluster/manager.go`)

```go
package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"adc-gateway/protocol"

	"github.com/redis/go-redis/v9"
)

const DeviceTTL = 70 * time.Second

type ClusterManager struct {
	NodeID        string
	RDB           *redis.Client
	pendingMu     sync.Mutex
	pendingReq    map[string]chan *protocol.ClusterCallResponse
	localExecutor func(ctx context.Context, tenantID, deviceID, toolName string, args map[string]interface{}) (*protocol.JSONRPCResponse, error)
}

func NewClusterManager(nodeID string, redisAddr string, password string) *ClusterManager {
	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: password,
		DB:       0,
	})

	return &ClusterManager{
		NodeID:     nodeID,
		RDB:        rdb,
		pendingReq: make(map[string]chan *protocol.ClusterCallResponse),
	}
}

func (c *ClusterManager) SetLocalExecutor(fn func(ctx context.Context, tenantID, deviceID, toolName string, args map[string]interface{}) (*protocol.JSONRPCResponse, error)) {
	c.localExecutor = fn
}

func (c *ClusterManager) Start(ctx context.Context) {
	reqChannel := fmt.Sprintf("adc:req:%s", c.NodeID)
	respChannel := fmt.Sprintf("adc:resp:%s", c.NodeID)

	pubsub := c.RDB.Subscribe(ctx, reqChannel, respChannel)
	go func() {
		defer pubsub.Close()
		ch := pubsub.Channel()

		for msg := range ch {
			if msg.Channel == reqChannel {
				go c.handleIncomingRequest(ctx, msg.Payload)
			} else if msg.Channel == respChannel {
				c.handleIncomingResponse(msg.Payload)
			}
		}
	}()
	log.Printf("[Cluster] Node [%s] subscribed to (%s, %s)", c.NodeID, reqChannel, respChannel)
}

func (c *ClusterManager) RegisterDevice(ctx context.Context, tenantID, deviceID string, tools []protocol.MCPTool) error {
	pipe := c.RDB.Pipeline()

	locKey := fmt.Sprintf("adc:loc:%s:%s", tenantID, deviceID)
	pipe.Set(ctx, locKey, c.NodeID, DeviceTTL)

	setKey := fmt.Sprintf("adc:tenant_devices:%s", tenantID)
	pipe.SAdd(ctx, setKey, deviceID)

	toolsBytes, _ := json.Marshal(tools)
	toolsKey := fmt.Sprintf("adc:tools:%s:%s", tenantID, deviceID)
	pipe.Set(ctx, toolsKey, toolsBytes, DeviceTTL)

	_, err := pipe.Exec(ctx)
	return err
}

func (c *ClusterManager) UnregisterDevice(ctx context.Context, tenantID, deviceID string) {
	locKey := fmt.Sprintf("adc:loc:%s:%s", tenantID, deviceID)
	toolsKey := fmt.Sprintf("adc:tools:%s:%s", tenantID, deviceID)
	setKey := fmt.Sprintf("adc:tenant_devices:%s", tenantID)

	c.RDB.Del(ctx, locKey, toolsKey)
	c.RDB.SRem(ctx, setKey, deviceID)
}

func (c *ClusterManager) GetTenantAggregatedTools(ctx context.Context, tenantID string) ([]protocol.MCPTool, error) {
	setKey := fmt.Sprintf("adc:tenant_devices:%s", tenantID)
	deviceIDs, err := c.RDB.SMembers(ctx, setKey).Result()
	if err != nil {
		return nil, err
	}

	aggregated := make([]protocol.MCPTool, 0)
	for _, devID := range deviceIDs {
		locKey := fmt.Sprintf("adc:loc:%s:%s", tenantID, devID)
		if exists, _ := c.RDB.Exists(ctx, locKey).Result(); exists == 0 {
			c.RDB.SRem(ctx, setKey, devID)
			continue
		}

		toolsKey := fmt.Sprintf("adc:tools:%s:%s", tenantID, devID)
		val, err := c.RDB.Get(ctx, toolsKey).Result()
		if err == nil {
			var tools []protocol.MCPTool
			if json.Unmarshal([]byte(val), &tools) == nil {
				for _, tool := range tools {
					aggregated = append(aggregated, protocol.MCPTool{
						Name:        fmt.Sprintf("%s__%s", devID, tool.Name),
						Description: fmt.Sprintf("[%s] %s", devID, tool.Description),
						InputSchema: tool.InputSchema,
					})
				}
			}
		}
	}
	return aggregated, nil
}

func (c *ClusterManager) RouteToolCall(ctx context.Context, req protocol.ClusterCallRequest) (*protocol.ClusterCallResponse, error) {
	locKey := fmt.Sprintf("adc:loc:%s:%s", req.TenantID, req.DeviceID)
	targetNodeID, err := c.RDB.Get(ctx, locKey).Result()
	if err != nil {
		return nil, errors.New("target device is offline or not found")
	}

	if targetNodeID == c.NodeID {
		if c.localExecutor == nil {
			return nil, errors.New("local executor not configured")
		}
		resp, err := c.localExecutor(ctx, req.TenantID, req.DeviceID, req.ToolName, req.Arguments)
		if err != nil {
			return nil, err
		}
		return &protocol.ClusterCallResponse{
			RequestID: req.RequestID,
			Result:    resp.Result,
			Error:     resp.Error,
		}, nil
	}

	req.FromNodeID = c.NodeID
	respChan := make(chan *protocol.ClusterCallResponse, 1)

	c.pendingMu.Lock()
	c.pendingReq[req.RequestID] = respChan
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pendingReq, req.RequestID)
		c.pendingMu.Unlock()
	}()

	payloadBytes, _ := json.Marshal(req)
	targetChannel := fmt.Sprintf("adc:req:%s", targetNodeID)
	if err := c.RDB.Publish(ctx, targetChannel, payloadBytes).Err(); err != nil {
		return nil, fmt.Errorf("publish failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respChan:
		return resp, nil
	}
}

func (c *ClusterManager) handleIncomingRequest(ctx context.Context, payload string) {
	var req protocol.ClusterCallRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		return
	}

	var callResp protocol.ClusterCallResponse
	callResp.RequestID = req.RequestID

	if c.localExecutor != nil {
		rpcResp, err := c.localExecutor(ctx, req.TenantID, req.DeviceID, req.ToolName, req.Arguments)
		if err != nil {
			callResp.Error = &protocol.JSONRPCError{Code: -32000, Message: err.Error()}
		} else {
			callResp.Result = rpcResp.Result
			callResp.Error = rpcResp.Error
		}
	} else {
		callResp.Error = &protocol.JSONRPCError{Code: -32603, Message: "executor not ready"}
	}

	respBytes, _ := json.Marshal(callResp)
	c.RDB.Publish(ctx, fmt.Sprintf("adc:resp:%s", req.FromNodeID), respBytes)
}

func (c *ClusterManager) handleIncomingResponse(payload string) {
	var resp protocol.ClusterCallResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return
	}

	c.pendingMu.Lock()
	ch, exists := c.pendingReq[resp.RequestID]
	c.pendingMu.Unlock()

	if exists {
		ch <- &resp
	}
}

```

---

## 6. 工业级安全与 HITL (Human-in-the-Loop) 审批拦截

针对工业高危指令，ADC 网关提供基于企业微信/钉钉交互卡片的风控闭环：

```text
[Agent 调度] ──► [ ADC Gateway: tools/call ]
                       │
                       ├─ 1. 拦截器识别 RiskLevel >= 2 (High-Risk)
                       ├─ 2. 生成审批工单 (Ticket UUID) 并写入 Redis
                       ├─ 3. 阻塞挂起 Agent 请求 (基于 Redis Pub/Sub 等待决策)
                       ▼
             [ 推送引擎: Webhook / API ]
                       │
                       ▼ 4. 发送交互式审批卡片
             [ 企业微信 / 钉钉 工作群 / 责任人 ]
                       │
                       ▼ 5. 工程师点击 [同意] 或 [拒绝]
             [ 企业微信/钉钉 回调服务器 ]
                       │
                       ▼ 6. POST /v1/hitl/callback
             [ ADC HITL Callback Handler ]
                       │
                       ├─ 7. 校验操作员身份与工单时效
                       ├─ 8. 更新工单状态 -> Redis Pub/Sub 广播决策
                       ▼
             [ ADC Gateway 唤醒挂起协程 ]
                       │
          ┌────────────┴────────────┐
       [同意]                     [拒绝 / 超时]
          ▼                         ▼
   下发指令给硬件执行          直接向 Agent 返回拦截错误

```

---

### 6.1 工单模型与状态机 (`hitl/models.go`)

```go
package hitl

import "time"

type RiskLevel int

const (
	RiskLevelReadSafe RiskLevel = 0 // 只读操作
	RiskLevelLowWrite RiskLevel = 1 // 低风险写入
	RiskLevelHigh     RiskLevel = 2 // 高危指令 (强制人工审批)
	RiskLevelCritical RiskLevel = 3 // 极危指令 (需物理闭环)
)

type ApprovalStatus string

const (
	StatusPending  ApprovalStatus = "PENDING"
	StatusApproved ApprovalStatus = "APPROVED"
	StatusRejected ApprovalStatus = "REJECTED"
	StatusExpired  ApprovalStatus = "EXPIRED"
)

type ApprovalTicket struct {
	TicketID   string                 `json:"ticket_id"`
	TenantID   string                 `json:"tenant_id"`
	AgentID    string                 `json:"agent_id"`
	DeviceID   string                 `json:"device_id"`
	ToolName   string                 `json:"tool_name"`
	Arguments  map[string]interface{} `json:"arguments"`
	RiskLevel  RiskLevel              `json:"risk_level"`
	Status     ApprovalStatus         `json:"status"`
	Approver   string                 `json:"approver,omitempty"`
	Comment    string                 `json:"comment,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
	ExpireAt   time.Time              `json:"expire_at"`
}

```

---

### 6.2 HITL 拦截管理器 (`hitl/manager.go`)

```go
package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type Notifier interface {
	SendApprovalCard(ctx context.Context, ticket *ApprovalTicket) error
}

type HITLManager struct {
	RDB          *redis.Client
	Notifier     Notifier
	pendingMu    sync.Mutex
	localWaiters map[string]chan *ApprovalTicket
}

func NewHITLManager(rdb *redis.Client, notifier Notifier) *HITLManager {
	return &HITLManager{
		RDB:          rdb,
		Notifier:     notifier,
		localWaiters: make(map[string]chan *ApprovalTicket),
	}
}

func (m *HITLManager) InterceptAndWait(ctx context.Context, tenantID, agentID, deviceID, toolName string, args map[string]interface{}, timeout time.Duration) (*ApprovalTicket, error) {
	ticketID := fmt.Sprintf("hitl_%s", uuid.New().String())
	now := time.Now()

	ticket := &ApprovalTicket{
		TicketID:  ticketID,
		TenantID:  tenantID,
		AgentID:   agentID,
		DeviceID:  deviceID,
		ToolName:  toolName,
		Arguments: args,
		RiskLevel: RiskLevelHigh,
		Status:    StatusPending,
		CreatedAt: now,
		ExpireAt:  now.Add(timeout),
	}

	ticketData, _ := json.Marshal(ticket)
	ticketKey := fmt.Sprintf("adc:hitl:ticket:%s", ticketID)
	if err := m.RDB.Set(ctx, ticketKey, ticketData, timeout+time.Minute).Err(); err != nil {
		return nil, fmt.Errorf("failed to save approval ticket: %w", err)
	}

	waitChan := make(chan *ApprovalTicket, 1)
	m.pendingMu.Lock()
	m.localWaiters[ticketID] = waitChan
	m.pendingMu.Unlock()

	defer func() {
		m.pendingMu.Lock()
		delete(m.localWaiters, ticketID)
		m.pendingMu.Unlock()
	}()

	go func() {
		_ = m.Notifier.SendApprovalCard(context.Background(), ticket)
	}()

	select {
	case <-ctx.Done():
		m.updateTicketStatus(context.Background(), ticketID, StatusExpired, "system", "Agent context cancelled")
		return nil, errors.New("request context cancelled while awaiting approval")

	case <-time.After(timeout):
		m.updateTicketStatus(context.Background(), ticketID, StatusExpired, "system", "Approval timed out")
		return nil, errors.New("HITL approval timeout: rejected for safety")

	case decision := <-waitChan:
		if decision.Status != StatusApproved {
			return decision, fmt.Errorf("operation rejected by operator [%s]: %s", decision.Approver, decision.Comment)
		}
		return decision, nil
	}
}

func (m *HITLManager) ResolveTicket(ctx context.Context, ticketID string, approved bool, approver, comment string) error {
	ticketKey := fmt.Sprintf("adc:hitl:ticket:%s", ticketID)
	val, err := m.RDB.Get(ctx, ticketKey).Result()
	if err != nil {
		return errors.New("approval ticket not found or already processed")
	}

	var ticket ApprovalTicket
	if err := json.Unmarshal([]byte(val), &ticket); err != nil {
		return err
	}

	if ticket.Status != StatusPending {
		return fmt.Errorf("ticket is already resolved: %s", ticket.Status)
	}

	status := StatusApproved
	if !approved {
		status = StatusRejected
	}

	ticket.Status = status
	ticket.Approver = approver
	ticket.Comment = comment

	ticketData, _ := json.Marshal(ticket)
	m.RDB.Set(ctx, ticketKey, ticketData, 24*time.Hour)

	m.pendingMu.Lock()
	ch, exists := m.localWaiters[ticketID]
	m.pendingMu.Unlock()

	if exists {
		ch <- &ticket
	} else {
		channel := fmt.Sprintf("adc:hitl:resolve:%s", ticketID)
		m.RDB.Publish(ctx, channel, ticketData)
	}

	return nil
}

func (m *HITLManager) updateTicketStatus(ctx context.Context, ticketID string, status ApprovalStatus, approver, comment string) {
	ticketKey := fmt.Sprintf("adc:hitl:ticket:%s", ticketID)
	val, err := m.RDB.Get(ctx, ticketKey).Result()
	if err == nil {
		var t ApprovalTicket
		if json.Unmarshal([]byte(val), &t) == nil {
			t.Status = status
			t.Approver = approver
			t.Comment = comment
			b, _ := json.Marshal(t)
			m.RDB.Set(ctx, ticketKey, b, 24*time.Hour)
		}
	}
}

```

---

### 6.3 钉钉与企业微信推送器 (`hitl/notifiers.go`)

```go
package hitl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// 企业微信卡片推送器
type WeComNotifier struct {
	WebhookURL string
	ServerURL  string
}

func (w *WeComNotifier) SendApprovalCard(ctx context.Context, ticket *ApprovalTicket) error {
	argsJSON, _ := json.MarshalIndent(ticket.Arguments, "", "  ")

	payload := map[string]interface{}{
		"msgtype": "template_card",
		"template_card": map[string]interface{}{
			"card_type": "button_interaction",
			"main_title": map[string]interface{}{
				"title": "🚨 拦截到高危工业操作请求",
				"desc":  fmt.Sprintf("Agent 正在申请调度设备 [%s]", ticket.DeviceID),
			},
			"horizontal_content_list": []map[string]interface{}{
				{"keyname": "目标设备", "value": ticket.DeviceID},
				{"keyname": "触发工具", "value": ticket.ToolName},
				{"keyname": "调用参数", "value": string(argsJSON)},
				{"keyname": "超时时间", "value": "5 分钟"},
			},
			"task_id": ticket.TicketID,
			"button_list": []map[string]interface{}{
				{
					"text":  "✅ 核实并执行",
					"style": 1,
					"key":   fmt.Sprintf("approve_%s", ticket.TicketID),
					"url":   fmt.Sprintf("%s/v1/hitl/action?ticket_id=%s&decision=approve", w.ServerURL, ticket.TicketID),
				},
				{
					"text":  "❌ 拦截终止",
					"style": 3,
					"key":   fmt.Sprintf("reject_%s", ticket.TicketID),
					"url":   fmt.Sprintf("%s/v1/hitl/action?ticket_id=%s&decision=reject", w.ServerURL, ticket.TicketID),
				},
			},
		},
	}

	return sendJSON(ctx, w.WebhookURL, payload)
}

// 钉钉 ActionCard 推送器
type DingTalkNotifier struct {
	WebhookURL string
	ServerURL  string
}

func (d *DingTalkNotifier) SendApprovalCard(ctx context.Context, ticket *ApprovalTicket) error {
	argsBytes, _ := json.Marshal(ticket.Arguments)

	markdown := fmt.Sprintf("### 🚨 ADC 工业指令审批请求\n\n"+
		"- **租户 ID**: `%s`\n"+
		"- **目标设备**: `%s`\n"+
		"- **下发工具**: `%s`\n"+
		"- **调用参数**: `%s`\n"+
		"- **风险等级**: `Level 2 (High Risk)`\n"+
		"> ⚠️ 请在 5 分钟内完成核实，超时将自动拒绝。",
		ticket.TenantID, ticket.DeviceID, ticket.ToolName, string(argsBytes))

	payload := map[string]interface{}{
		"msgtype": "actionCard",
		"actionCard": map[string]interface{}{
			"title":          "ADC 高危硬件操作审批",
			"text":           markdown,
			"btnOrientation": "0",
			"btns": []map[string]string{
				{
					"title":     "✅ 确认放行",
					"actionURL": fmt.Sprintf("%s/v1/hitl/action?ticket_id=%s&decision=approve", d.ServerURL, ticket.TicketID),
				},
				{
					"title":     "❌ 拒绝执行",
					"actionURL": fmt.Sprintf("%s/v1/hitl/action?ticket_id=%s&decision=reject", d.ServerURL, ticket.TicketID),
				},
			},
		},
	}

	return sendJSON(ctx, d.WebhookURL, payload)
}

func sendJSON(ctx context.Context, url string, payload interface{}) error {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

```

---

## 7. 生产网关集成与完整服务入口

### 7.1 集群路由与 HITL 拦截整合 (`gateway/handler.go`)

```go
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"adc-gateway/auth"
	"adc-gateway/cluster"
	"adc-gateway/hitl"
	"adc-gateway/hub"
	"adc-gateway/protocol"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type ProductionGatewayHandler struct {
	Hub     *hub.DeviceHub
	Auth    *auth.Authenticator
	Cluster *cluster.ClusterManager
	HITL    *hitl.HITLManager
}

func NewProductionGatewayHandler(h *hub.DeviceHub, a *auth.Authenticator, c *cluster.ClusterManager, m *hitl.HITLManager) *ProductionGatewayHandler {
	handler := &ProductionGatewayHandler{
		Hub:     h,
		Auth:    a,
		Cluster: c,
		HITL:    m,
	}

	c.SetLocalExecutor(func(ctx context.Context, tenantID, deviceID, toolName string, args map[string]interface{}) (*protocol.JSONRPCResponse, error) {
		dev, err := h.GetDevice(tenantID, deviceID)
		if err != nil {
			return nil, err
		}
		return dev.SendRPC(ctx, "tools/call", protocol.ToolCallParams{
			Name:      toolName,
			Arguments: args,
		})
	})

	return handler
}

// 1. 设备接入反向 WebSocket 隧道
func (gh *ProductionGatewayHandler) ServeDeviceTunnel(w http.ResponseWriter, r *http.Request) {
	deviceID := r.Header.Get("X-Device-ID")
	token := r.Header.Get("X-Device-Token")
	cred, err := gh.Auth.Verify(deviceID, token)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	session := hub.NewDeviceSession(cred.DeviceID, cred.TenantID, conn)
	gh.Hub.Register(session)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		resp, err := session.SendRPC(ctx, "tools/list", nil)
		if err == nil && resp.Error == nil {
			var listRes protocol.ToolsListResult
			if json.Unmarshal(resp.Result, &listRes) == nil {
				session.SetTools(listRes.Tools)
				_ = gh.Cluster.RegisterDevice(context.Background(), cred.TenantID, cred.DeviceID, listRes.Tools)
				log.Printf("[Gateway] Synced %d tools for device %s", len(listRes.Tools), cred.DeviceID)
			}
		}
	}()

	session.StartPump(func() {
		gh.Hub.Unregister(cred.TenantID, cred.DeviceID)
		gh.Cluster.UnregisterDevice(context.Background(), cred.TenantID, cred.DeviceID)
		log.Printf("[Gateway] Device %s disconnected", cred.DeviceID)
	})
}

// 2. Agent 统一查询工具端点
func (gh *ProductionGatewayHandler) HandleAgentToolsList(w http.ResponseWriter, r *http.Request) {
	tenantID := r.Header.Get("X-Tenant-ID")
	if tenantID == "" {
		http.Error(w, `{"error":"missing X-Tenant-ID"}`, http.StatusBadRequest)
		return
	}

	tools, err := gh.Cluster.GetTenantAggregatedTools(r.Context(), tenantID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(protocol.ToolsListResult{Tools: tools})
}

// 3. Agent 统一工具调用端点 (集成风控与 HITL 拦截)
func (gh *ProductionGatewayHandler) HandleAgentToolCall(w http.ResponseWriter, r *http.Request) {
	tenantID := r.Header.Get("X-Tenant-ID")
	if tenantID == "" {
		http.Error(w, `{"error":"missing X-Tenant-ID"}`, http.StatusBadRequest)
		return
	}

	var callReq protocol.ToolCallParams
	if err := json.NewDecoder(r.Body).Decode(&callReq); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	parts := strings.SplitN(callReq.Name, "__", 2)
	if len(parts) != 2 {
		http.Error(w, `{"error":"invalid tool name format"}`, http.StatusBadRequest)
		return
	}
	targetDeviceID, targetToolName := parts[0], parts[1]

	// 工业风控拦截：高危工具进入 HITL 审批
	if isHighRiskOperation(targetToolName) {
		log.Printf("[HITL Intercept] High risk tool %s requested on %s", targetToolName, targetDeviceID)
		_, err := gh.HITL.InterceptAndWait(
			r.Context(),
			tenantID,
			"agent-production-worker",
			targetDeviceID,
			targetToolName,
			callReq.Arguments,
			5*time.Minute,
		)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(protocol.ToolCallResult{
				Content: []protocol.ToolContent{
					{Type: "text", Text: fmt.Sprintf("BLOCKED_BY_HITL: %s", err.Error())},
				},
				IsError: true,
			})
			return
		}
	}

	// 审批通过或免审操作，集群调度执行
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	clusterResp, err := gh.Cluster.RouteToolCall(ctx, protocol.ClusterCallRequest{
		RequestID: uuid.New().String(),
		TenantID:  tenantID,
		DeviceID:  targetDeviceID,
		ToolName:  targetToolName,
		Arguments: callReq.Arguments,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusGatewayTimeout)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if clusterResp.Error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(clusterResp.Error)
		return
	}

	w.Write(clusterResp.Result)
}

// 4. 审批回调端点
func (gh *ProductionGatewayHandler) HandleHITLAction(w http.ResponseWriter, r *http.Request) {
	ticketID := r.URL.Query().Get("ticket_id")
	decision := r.URL.Query().Get("decision")

	if ticketID == "" || (decision != "approve" && decision != "reject") {
		http.Error(w, "invalid params", http.StatusBadRequest)
		return
	}

	isApproved := decision == "approve"
	err := gh.HITL.ResolveTicket(r.Context(), ticketID, isApproved, "DutyEngineer", "Resolved via ChatOps Card")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, "<h2 style='color:red;'>审批失败或已过期: %v</h2>", err)
		return
	}

	if isApproved {
		fmt.Fprintf(w, "<h2 style='color:green;'>✅ 操作已批准，ADC 网关正在下发指令...</h2>")
	} else {
		fmt.Fprintf(w, "<h2 style='color:orange;'>❌ 操作已被成功拦截终止。</h2>")
	}
}

func isHighRiskOperation(toolName string) bool {
	highRiskKeywords := []string{"set_spindle_speed", "adjust_temperature", "stop", "reboot", "override"}
	for _, kw := range highRiskKeywords {
		if strings.Contains(toolName, kw) {
			return true
		}
	}
	return false
}

```

---

### 7.2 服务启动入口 (`main.go`)

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"adc-gateway/auth"
	"adc-gateway/cluster"
	"adc-gateway/gateway"
	"adc-gateway/hitl"
	"adc-gateway/hub"
)

func main() {
	var (
		port       int
		nodeID     string
		redisAddr  string
		webhookURL string
		serverURL  string
	)
	flag.IntVar(&port, "port", 8080, "Gateway HTTP Port")
	flag.StringVar(&nodeID, "node-id", "adc-node-01", "Node ID")
	flag.StringVar(&redisAddr, "redis", "127.0.0.1:6379", "Redis Address")
	flag.StringVar(&webhookURL, "webhook", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY", "ChatOps Webhook")
	flag.StringVar(&serverURL, "server-url", "http://localhost:8080", "Gateway External URL")
	flag.Parse()

	// 1. 初始化核心模块
	authenticator := auth.NewAuthenticator()
	deviceHub := hub.NewDeviceHub()
	clusterMgr := cluster.NewClusterManager(nodeID, redisAddr, "")

	notifier := &hitl.WeComNotifier{
		WebhookURL: webhookURL,
		ServerURL:  serverURL,
	}
	hitlMgr := hitl.NewHITLManager(clusterMgr.RDB, notifier)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clusterMgr.Start(ctx)

	// 2. 注册路由
	handler := gateway.NewProductionGatewayHandler(deviceHub, authenticator, clusterMgr, hitlMgr)
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/devices/tunnel", handler.ServeDeviceTunnel)
	mux.HandleFunc("/v1/agent/mcp/tools", handler.HandleAgentToolsList)
	mux.HandleFunc("/v1/agent/mcp/tools/call", handler.HandleAgentToolCall)
	mux.HandleFunc("/v1/hitl/action", handler.HandleHITLAction)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		log.Printf("ADC Production Gateway [%s] listening on :%d", nodeID, port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server startup failed: %v", err)
		}
	}()

	// 3. 优雅退出
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down ADC Gateway...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	_ = server.Shutdown(shutdownCtx)
	log.Println("ADC Gateway gracefully stopped.")
}

```

---

## 8. 嵌入式硬件模拟端实现 (`mock_device/device.go`)

用于快速联调测试硬件反向接入与 MCP 执行：

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"adc-gateway/protocol"

	"github.com/gorilla/websocket"
)

func main() {
	serverURL := "ws://localhost:8080/v1/devices/tunnel"
	deviceID := "cnc-lathe-01"
	token := "sec_cnc_secret_key_8899"

	headers := http.Header{}
	headers.Set("X-Device-ID", deviceID)
	headers.Set("X-Device-Token", token)

	log.Printf("[Hardware] Connecting to ADC Gateway %s...", serverURL)
	conn, _, err := websocket.DefaultDialer.Dial(serverURL, headers)
	if err != nil {
		log.Fatalf("[Hardware] Connection failed: %v", err)
	}
	defer conn.Close()

	log.Println("[Hardware] Reverse tunnel online. Ready for MCP calls.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		for {
			var req protocol.JSONRPCRequest
			if err := conn.ReadJSON(&req); err != nil {
				return
			}

			switch req.Method {
			case "tools/list":
				resp := protocol.JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result: mustJSON(protocol.ToolsListResult{
						Tools: []protocol.MCPTool{
							{
								Name:        "get_spindle_status",
								Description: "Read CNC spindle RPM & temperature (Read-Only)",
								InputSchema: json.RawMessage(`{"type":"object"}`),
							},
							{
								Name:        "set_spindle_speed",
								Description: "Set CNC spindle RPM (High Risk - Requires HITL)",
								InputSchema: json.RawMessage(`{"type":"object","properties":{"rpm":{"type":"number"}},"required":["rpm"]}`),
							},
						},
					}),
				}
				conn.WriteJSON(resp)

			case "tools/call":
				var params protocol.ToolCallParams
				rawBytes, _ := json.Marshal(req.Params)
				json.Unmarshal(rawBytes, &params)

				var resultText string
				if params.Name == "get_spindle_status" {
					resultText = `{"status":"RUNNING","rpm":4200,"temperature_celsius":38.2}`
				} else if params.Name == "set_spindle_speed" {
					resultText = fmt.Sprintf("Spindle speed set to %v RPM successfully", params.Arguments["rpm"])
				}

				resp := protocol.JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result: mustJSON(protocol.ToolCallResult{
						Content: []protocol.ToolContent{{Type: "text", Text: resultText}},
						IsError: false,
					}),
				}
				conn.WriteJSON(resp)
			}
		}
	}()

	<-sigChan
	log.Println("[Hardware] Device offline.")
}

func mustJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

```

---

## 9. 方案实施与落地演进路线

```text
[ 阶段一：核心协议与反向穿透 (MVP) ]
  ├── 落地反向 WebSocket 隧道与设备 Token 鉴权
  └── 实现 Agent 虚拟 MCP 端点聚合与 JSON-RPC 2.0 异步调用
           │
           ▼
[ 阶段二：工业安全与集群扩展 (Production Ready) ]
  ├── 引入 Redis Pub/Sub 实现多实例集群化调度
  └── 上线 HITL 风控引擎，打通企业微信/钉钉审批卡片
           │
           ▼
[ 阶段三：边缘生态与模型治理 (Ecosystem) ]
  ├── 发布 C / Rust 轻量级嵌入式 MCP SDK (适配 ESP32 / Linux 盒子)
  └── 集成 LiteLLM / Langfuse 统一模型网关，实现 Token 计量与 Trace 审计

```
