# 变更：方案 A 落地——统一 API 网关与 Python Agent 面

## Why

技术栈评审（design/70）经审批采纳方案 A：Go 数据面 + Python Agent 面 + 统一 API 网关。当前仓库仅有 Go 数据面模块（ce/internal/*，phase0 进行中），缺少：① 统一 API 网关（前端唯一入口，后端 Go/Python 无感切换）；② Python Agent 面（V1.0 最小集：评测工具链）；③ core-sdk 双语言协议包。本变更将方案 A 的架构决策落地为代码。

## What Changes

- 新增 ce/internal/gateway（Go）：统一 API 网关——前端唯一入口；路径前缀路由（/v1/admin/*→Admin API、/v1/agent/*→Agent API、/v1/devices/tunnel→Connector、/v1/hitl/*→Approval、/v2/agents/*→Python Agent 面预留）；鉴权透传、限流复用、trace_id 贯通、OpenAPI 契约校验
- 新增 ce/py-agent（Python 3.14 + FastAPI/uvicorn）：V1.0 最小集 = 评测工具链（EvalHarness，CLI + API 双入口）；V1.5 完整集（LLM 网关/四 Agent 编排/A2A Agent Card）仅预留目录与依赖，不实现
- 新增 core-sdk/python（Python 版协议包）：与 core-sdk/protocol（Go）同一契约，由 OpenAPI/JSON Schema 驱动
- ce/internal/httpx 增强：网关级中间件（HTTP 超时、body 限制、限流中间件挂载点）
- 前端（未来控制台工程）baseURL 规范：只配置网关地址（契约规范写入 design/20 引用，代码在后续变更）

## Capabilities

- **New Capabilities**: `api-gateway`（统一网关：路由/透传/限流/契约校验/前端无感）、`python-agent-plane`（Python Agent 面：V1.0 评测工具链最小集）
- **Modified Capabilities**: 无

## Impact

- 新增：ce/internal/gateway、ce/py-agent、core-sdk/python
- 修改：ce/internal/httpx（中间件）、go.work/go.mod（ce 依赖 core-sdk 已替换）、AGENTS.md（项目结构说明）
- 非目标：V1.5 完整 Python Agent 面（LLM 网关/四 Agent 编排/A2A）不在本变更实现；前端控制台工程不在本变更；NATS/OPA 不在本变更
- 依赖：phase0-security-hardening（网关复用其 auth/ratelimit/audit 模块）
